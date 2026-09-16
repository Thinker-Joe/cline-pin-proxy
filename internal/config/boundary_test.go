package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// ---------------------------------------------------------------------------
// 边界回归测试（来源：docs/CODE_REVIEW.md 的独立审查）。
// ---------------------------------------------------------------------------

// 文件里出现 rules 时，必须先解码成全新的切片。
//
// Load 从 Default() 出发整体覆盖，而 Go 的 json 解码器会复用已有 slice 的
// 元素：规则里没写的字段会保留**同位置默认规则**的值。于是
// {"rules":[{"model":"custom-model"}]} 会悄悄变成
// Name=deepseek, Upstreams=[deepseek] —— 把自定义模型发去官方渠道。
func TestRulesDoNotInheritDefaultElements(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	writeFile(t, path, `{"rules":[{"model":"custom-model"}]}`)

	cfg, err := Load(path)
	if err == nil {
		t.Fatalf("缺 upstreams 的规则必须被拒绝，实际加载成 %+v", cfg.Rules)
	}
	if !strings.Contains(err.Error(), "upstreams") {
		t.Errorf("错误信息应指向 upstreams，实际: %v", err)
	}
}

// 只配进程级字段的文件必须保留默认规则表（不能因为改动而改变既有行为）。
func TestFileWithoutRulesKeepsDefaultTable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	writeFile(t, path, `{"listen":"127.0.0.1:9999"}`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Rules) != len(Default().Rules) {
		t.Errorf("规则数 = %d, want %d", len(cfg.Rules), len(Default().Rules))
	}
}

// 显式空数组是合法的"关闭钉死"，不能回落到默认规则。
func TestExplicitEmptyRulesDisablesPinning(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	writeFile(t, path, `{"rules":[]}`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Rules) != 0 {
		t.Errorf("规则数 = %d, want 0", len(cfg.Rules))
	}
	if _, matched := cfg.Match("cline-pass/glm-5.3"); matched {
		t.Error("显式空规则表不应命中任何模型")
	}
}

// JSON null 顶层不是合法配置。它解码到 *Config 是 no-op（保持默认值），
// 解码到 map 则得到 nil map，随后 doc["rules"]=... 直接 panic。
func TestNullConfigIsRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	writeFile(t, path, `null`)

	if _, err := Load(path); err == nil {
		t.Error("顶层为 null 的配置必须被拒绝")
	}
	if _, err := NewStore(path, nil); err == nil {
		t.Error("NewStore 也必须拒绝 null 配置")
	}
}

// 显式给了 CLINE_PIN_RULES 但内容非法，不能等同于"没配置"。
//
// 静默回退会让运维者以为规则表已被覆盖，实际继续走默认的第三方 GLM 渠道。
func TestInvalidEnvRulesIsAnError(t *testing.T) {
	t.Setenv("CLINE_PIN_RULES", `[{invalid`)

	if _, err := Load(""); err == nil {
		t.Error("非法的 CLINE_PIN_RULES 必须让加载失败，而不是静默用默认规则")
	}
}

func TestValidEnvRulesStillOverride(t *testing.T) {
	t.Setenv("CLINE_PIN_RULES", `[{"name":"env","model":"m","upstreams":["x"]}]`)

	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Rules) != 1 || cfg.Rules[0].Name != "env" {
		t.Errorf("Rules = %+v", cfg.Rules)
	}
}

// 落盘只替换 rules 键，其余字段必须逐字保留——包括超出 float64 精度的大整数。
func TestPersistPreservesLargeIntegers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	const big = "9007199254740993" // 2^53+1，float64 无法精确表示
	writeFile(t, path, `{"max_body_bytes":`+big+`,"rules":[{"name":"a","model":"m","upstreams":["x"]}]}`)

	s, err := NewStore(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, persistErr, err := s.SetRules([]Rule{{Name: "b", Model: "m", Upstreams: []string{"y"}}}); err != nil || persistErr != nil {
		t.Fatalf("SetRules: err=%v persistErr=%v", err, persistErr)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), big) {
		t.Errorf("大整数被改写，文件内容:\n%s", raw)
	}
}

// upstream 只看前缀会放行不可用的地址：非法转义要到真实请求才炸，
// 带 query 的基址会被字符串拼接毁掉。
func TestUpstreamValidationRejectsUnusableURLs(t *testing.T) {
	for _, bad := range []string{
		"http://%",
		"http://localhost?x=1",
		"http://localhost/path#frag",
		"https://",
		"ftp://example.com",
	} {
		cfg := Default()
		cfg.Upstream = bad
		if err := cfg.Normalize(); err == nil {
			t.Errorf("upstream %q 应被拒绝，实际通过（归一化后 %q）", bad, cfg.Upstream)
		}
	}

	for _, ok := range []string{
		"https://api.cline.bot/api/v1",
		"http://cline-pin-proxy:8787/v1",
		"https://user:pass@example.com/api/v1",
	} {
		cfg := Default()
		cfg.Upstream = ok
		if err := cfg.Normalize(); err != nil {
			t.Errorf("upstream %q 应被接受: %v", ok, err)
		}
	}
}

// SetRules 落盘时读的是**当前磁盘文件**，若此时内存还停在旧版本，
// 就把外部修改（例如换掉的 admin_token）永久吞掉了：
// 磁盘是新值、内存是旧值，且 mtime 被标记为已处理，热重载永远不会再来一次。
func TestSetRulesDoesNotConsumePendingExternalEdit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	writeFile(t, path, `{"admin_token":"old","rules":[{"name":"a","model":"m","upstreams":["x"]}]}`)

	s, err := NewStore(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Current().AdminToken; got != "old" {
		t.Fatalf("初始 admin_token = %q", got)
	}

	// 外部改文件——下一次轮询之前调用 SetRules。
	writeFile(t, path, `{"admin_token":"new","rules":[{"name":"a","model":"m","upstreams":["x"]}]}`)

	if _, persistErr, err := s.SetRules([]Rule{{Name: "b", Model: "m", Upstreams: []string{"y"}}}); err != nil || persistErr != nil {
		t.Fatalf("SetRules: err=%v persistErr=%v", err, persistErr)
	}

	if got := s.Current().AdminToken; got != "new" {
		t.Errorf("内存 admin_token = %q, want new —— 外部修改被 SetRules 吞掉了", got)
	}
	if got := s.Current().Rules; len(got) != 1 || got[0].Name != "b" {
		t.Errorf("规则 = %+v, want 新写入的 b", got)
	}
}

// 原子替换失败的**原因**必须分类：只有"这个目录做不到"才该退化为原地覆盖。
//
// 内容写入/同步失败（磁盘满、I/O 错误）时再 O_TRUNC 截断唯一的配置文件，
// 会把唯一完好的副本也毁掉——既没写成功，又失去重建依据。
func TestAtomicFallbackIsRestrictedToUnavailableCases(t *testing.T) {
	unavailable := []error{
		&os.PathError{Op: "rename", Path: "/x", Err: os.ErrPermission},
		&os.LinkError{Op: "rename", Old: "/a", New: "/b", Err: os.ErrPermission},
		&os.PathError{Op: "open", Path: "/x", Err: syscall.EROFS},
		&os.PathError{Op: "rename", Path: "/x", Err: syscall.EXDEV},
		syscall.EACCES,
		// 不支持类错误统一走 errors.ErrUnsupported（见函数注释：不能在 switch 里
		// 同时列 ENOTSUP 与 EOPNOTSUPP，Linux 上那是重复 case，直接编译失败）。
		&os.PathError{Op: "rename", Path: "/x", Err: syscall.ENOSYS},
		errors.ErrUnsupported,
	}
	contentFailures := []error{
		&os.PathError{Op: "write", Path: "/x", Err: syscall.ENOSPC},
		&os.PathError{Op: "sync", Path: "/x", Err: syscall.EIO},
		&os.PathError{Op: "write", Path: "/x", Err: os.ErrClosed},
		errors.New("some other failure"),
		nil,
	}

	for _, err := range unavailable {
		if !isAtomicReplaceUnavailable(err) {
			t.Errorf("%v 应被判为「原子替换不可用」", err)
		}
	}
	for _, err := range contentFailures {
		if isAtomicReplaceUnavailable(err) {
			t.Errorf("%v 不应触发原地覆盖退路（会把唯一好文件截断）", err)
		}
	}
}

// 写回失败时不能破坏原有文件。
func TestFailedPersistLeavesFileIntact(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	original := `{"admin_token":"keepme","rules":[{"name":"a","model":"m","upstreams":["x"]}]}`
	writeFile(t, path, original)

	s, err := NewStore(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	// 指向一个不存在的目录 -> 两条写回路径都失败。
	s.path = filepath.Join(dir, "no-such-dir", "config.json")

	persisted, persistErr, err := s.SetRules([]Rule{{Name: "b", Model: "m", Upstreams: []string{"y"}}})
	if err != nil {
		t.Fatalf("SetRules 不应硬失败: %v", err)
	}
	if persisted || persistErr == nil {
		t.Fatalf("persisted=%v persistErr=%v, want false/<error>", persisted, persistErr)
	}

	raw, readErr := os.ReadFile(filepath.Join(dir, "config.json"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(raw) != original {
		t.Errorf("原文件被改动:\n%s", raw)
	}
}
