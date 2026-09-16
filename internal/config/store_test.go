package config

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	// 拉开两次写入的修改时间，避免同一时间戳让"是否变化"的判断失真。
	time.Sleep(15 * time.Millisecond)
}

func readDoc(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	doc := map[string]any{}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("config file is not valid JSON: %v\n%s", err, raw)
	}
	return doc
}

func TestStaticSource(t *testing.T) {
	cfg := Default()
	s := Static(cfg)
	if got := s.Current(); got != cfg {
		t.Errorf("Static should return the exact config given")
	}
	if s.Path() != "" {
		t.Errorf("Static store should have no path, got %q", s.Path())
	}
	// 不影响入参：SetRules 换的是 store 里的指针，不是调用方的 cfg。
	_, _, err := s.SetRules([]Rule{{Name: "x", Model: "m", Upstreams: []string{"a"}}})
	if err != nil {
		t.Fatalf("SetRules on static store: %v", err)
	}
	if len(cfg.Rules) == 1 && cfg.Rules[0].Name == "x" {
		t.Error("SetRules must not mutate the caller's config")
	}
}

func TestNilAndZeroStoreFallBackToDefault(t *testing.T) {
	var s *Store
	if s.Current() == nil {
		t.Fatal("nil store should still return a usable config")
	}
	var zero Store
	if zero.Current() == nil {
		t.Fatal("zero store should still return a usable config")
	}
}

func TestNewStoreLoadsFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	writeFile(t, path, `{"listen":"127.0.0.1:9999","rules":[{"name":"a","model":"deepseek","upstreams":["deepseek"]}]}`)

	s, err := NewStore(path, nil)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if s.Path() != path {
		t.Errorf("Path = %q", s.Path())
	}
	if s.Current().Listen != "127.0.0.1:9999" {
		t.Errorf("Listen = %q", s.Current().Listen)
	}
	if len(s.Current().Rules) != 1 || s.Current().Rules[0].Name != "a" {
		t.Errorf("Rules = %+v", s.Current().Rules)
	}
}

func TestNewStoreFailsOnBadFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte("{oops"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(path, nil); err == nil {
		t.Fatal("expected an error for a malformed config")
	}
}

// 文件缺失不该让进程起不来：Docker 首次 `compose up` 时 config.json
// 往往还不存在（bind mount 还需用户自己放）。以默认值启动，并保留路径，
// 这样用户随后把文件放进去就能被热重载发现。
func TestNewStoreMissingFileStartsFromDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-yet.json")

	s, err := NewStore(path, nil)
	if err != nil {
		t.Fatalf("NewStore on a missing file should not fail: %v", err)
	}
	if s.Path() != path {
		t.Errorf("Path = %q, want %q (kept so the file can appear later)", s.Path(), path)
	}
	if got, want := s.Current().Listen, Default().Listen; got != want {
		t.Errorf("Listen = %q, want default %q", got, want)
	}

	// 文件后来被创建出来 -> 热重载应当接管。
	writeFile(t, path, `{"listen":"127.0.0.1:9911","rules":[{"name":"late","model":"glm-5.3","upstreams":["friendli"]}]}`)
	if _, err := s.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if got := s.Current().Listen; got != "127.0.0.1:9911" {
		t.Errorf("Listen after creating the file = %q", got)
	}
	if len(s.Current().Rules) != 1 || s.Current().Rules[0].Name != "late" {
		t.Errorf("Rules = %+v", s.Current().Rules)
	}
}

// 首次写回时配置文件可能还不存在：这时应当直接创建，而不是报错。
func TestSetRulesCreatesMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "created-by-api.json")

	s, err := NewStore(path, nil)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	persisted, persistErr, err := s.SetRules([]Rule{{Name: "fresh", Model: "glm-5.3", Upstreams: []string{"friendli"}}})
	if err != nil {
		t.Fatalf("SetRules: %v", err)
	}
	if !persisted || persistErr != nil {
		t.Fatalf("persisted=%v persistErr=%v, want true/nil", persisted, persistErr)
	}
	doc := readDoc(t, path)
	rules, _ := doc["rules"].([]any)
	if len(rules) != 1 {
		t.Fatalf("rules in created file = %#v", doc["rules"])
	}
	if first, _ := rules[0].(map[string]any); first["name"] != "fresh" {
		t.Errorf("rule = %#v", rules[0])
	}
}

// 热重载是本次迭代的核心：改配置文件即生效，Docker 下不必重建容器。
func TestReloadIfChangedPicksUpEdits(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	writeFile(t, path, `{"rules":[{"name":"old","model":"deepseek","upstreams":["deepseek"]}]}`)

	s, err := NewStore(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if s.Current().Rules[0].Name != "old" {
		t.Fatalf("initial rule = %q", s.Current().Rules[0].Name)
	}

	// 未改动时不该报告变化。
	if changed, err := s.ReloadIfChanged(); err != nil || changed {
		t.Fatalf("ReloadIfChanged with no edit = (%v, %v), want (false, nil)", changed, err)
	}

	writeFile(t, path, `{"rules":[{"name":"new","model":"glm-5.3","upstreams":["friendli"]}]}`)
	changed, err := s.ReloadIfChanged()
	if err != nil {
		t.Fatalf("ReloadIfChanged: %v", err)
	}
	if !changed {
		t.Fatal("ReloadIfChanged should report a change after the file was edited")
	}
	rule := s.Current().Rules[0]
	if rule.Name != "new" || rule.Upstreams[0] != "friendli" {
		t.Errorf("reloaded rule = %+v", rule)
	}
}

// 配置写坏时不能让代理裸奔：保留上一份可用配置。
func TestReloadKeepsPreviousConfigOnParseError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	writeFile(t, path, `{"rules":[{"name":"good","model":"deepseek","upstreams":["deepseek"]}]}`)

	s, err := NewStore(path, nil)
	if err != nil {
		t.Fatal(err)
	}

	writeFile(t, path, `{broken`)
	if _, err := s.ReloadIfChanged(); err == nil {
		t.Fatal("expected an error for malformed config")
	}
	if s.Current().Rules[0].Name != "good" {
		t.Errorf("previous config should survive a failed reload, got %q", s.Current().Rules[0].Name)
	}
}

// 文件被删除（例如改名替换的中间态）时保留当前配置。
func TestReloadKeepsConfigWhenFileMissing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	writeFile(t, path, `{"rules":[{"name":"good","model":"deepseek","upstreams":["deepseek"]}]}`)

	s, err := NewStore(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReloadIfChanged(); err == nil {
		t.Fatal("expected an error when the file is gone")
	}
	if s.Current().Rules[0].Name != "good" {
		t.Error("config should survive a missing file")
	}
}

func TestSetRulesAppliesImmediately(t *testing.T) {
	s, err := NewStore("", nil)
	if err != nil {
		t.Fatal(err)
	}
	persisted, persistErr, err := s.SetRules([]Rule{
		{Name: "kimi", Model: "kimi", Upstreams: []string{"novita"}},
	})
	if err != nil {
		t.Fatalf("SetRules: %v", err)
	}
	if persisted {
		t.Error("no config file -> persisted should be false")
	}
	if persistErr != nil {
		t.Errorf("no config file is not a persist error, got %v", persistErr)
	}
	if r, ok := s.Current().Match("cline-pass/kimi-k3"); !ok || r.Name != "kimi" {
		t.Errorf("rules not applied: %+v", s.Current().Rules)
	}
}

func TestSetRulesRejectsInvalid(t *testing.T) {
	s := Static(Default())
	before := len(s.Current().Rules)

	if _, _, err := s.SetRules([]Rule{{Name: "bad", Model: "", Upstreams: []string{"x"}}}); err == nil {
		t.Fatal("expected validation error for empty model")
	}
	if len(s.Current().Rules) != before {
		t.Error("invalid rules must not be applied")
	}
}

// 写回只替换 rules 键，其余内容原样保留。
func TestSetRulesPersistsWithoutTouchingOtherKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	writeFile(t, path, `{
  "listen": "127.0.0.1:8787",
  "upstream": "https://example.test/api/v1",
  "probe_headers": {"x-client-type": "cline-cli"},
  "rules": [{"name":"old","model":"deepseek","upstreams":["deepseek"]}]
}`)

	s, err := NewStore(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	persisted, persistErr, err := s.SetRules([]Rule{
		{Name: "new", Model: "glm-5.3", Upstreams: []string{"friendli"}},
	})
	if err != nil || persistErr != nil {
		t.Fatalf("SetRules: err=%v persistErr=%v", err, persistErr)
	}
	if !persisted {
		t.Fatal("expected the rules to be persisted")
	}

	doc := readDoc(t, path)
	if doc["listen"] != "127.0.0.1:8787" {
		t.Errorf("listen changed: %v", doc["listen"])
	}
	if doc["upstream"] != "https://example.test/api/v1" {
		t.Errorf("upstream changed: %v", doc["upstream"])
	}
	if _, ok := doc["probe_headers"]; !ok {
		t.Error("probe_headers was dropped")
	}
	rules, ok := doc["rules"].([]any)
	if !ok || len(rules) != 1 {
		t.Fatalf("rules = %#v", doc["rules"])
	}
	if name := rules[0].(map[string]any)["name"]; name != "new" {
		t.Errorf("rules not replaced in file: %v", name)
	}

	// 文件被重写后，store 不应误以为还需要再重载一次。
	if changed, err := s.ReloadIfChanged(); err != nil || changed {
		t.Errorf("ReloadIfChanged after persist = (%v, %v), want (false, nil)", changed, err)
	}
}

// 写回绝不能把环境变量的覆盖值烤进文件。
func TestSetRulesPersistDoesNotBakeEnvOverrides(t *testing.T) {
	t.Setenv("CLINE_PIN_UPSTREAM", "https://from-env.test/api/v1")
	t.Setenv("CLINE_PIN_API_KEY", "sk-from-env")

	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	writeFile(t, path, `{"upstream":"https://from-file.test/api/v1","rules":[{"name":"old","model":"deepseek","upstreams":["deepseek"]}]}`)

	s, err := NewStore(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if s.Current().Upstream != "https://from-env.test/api/v1" {
		t.Fatalf("env override not applied: %q", s.Current().Upstream)
	}

	if _, persistErr, err := s.SetRules([]Rule{{Name: "n", Model: "m", Upstreams: []string{"a"}}}); err != nil || persistErr != nil {
		t.Fatalf("SetRules: err=%v persistErr=%v", err, persistErr)
	}

	doc := readDoc(t, path)
	if doc["upstream"] != "https://from-file.test/api/v1" {
		t.Errorf("env value leaked into the file: %v", doc["upstream"])
	}
	if _, leaked := doc["api_key"]; leaked {
		t.Error("api_key from env must not be written into the config file")
	}
}

// 只读挂载是常见部署形态，写回失败必须被如实报告而不是静默吞掉。
func TestSetRulesReportsPersistFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	writeFile(t, path, `{"rules":[{"name":"old","model":"deepseek","upstreams":["deepseek"]}]}`)

	s, err := NewStore(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	// 让目录不可写，模拟只读挂载（Windows 上 chmod 语义不同，用只读文件代替）。
	if err := os.Chmod(path, 0o444); err != nil {
		t.Skipf("cannot chmod on this platform: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o644) })

	persisted, persistErr, err := s.SetRules([]Rule{{Name: "new", Model: "m", Upstreams: []string{"a"}}})
	if err != nil {
		t.Fatalf("SetRules should not fail hard: %v", err)
	}
	// 以 root 运行时写只读文件仍会成功，此时跳过断言。
	if persisted {
		t.Skip("running with privileges that bypass read-only files")
	}
	if persistErr == nil {
		t.Fatal("expected a persist error to be reported")
	}
	// 关键：写回失败不影响运行期生效。
	if s.Current().Rules[0].Name != "new" {
		t.Error("rules should still apply in memory when persist fails")
	}
}

// 原地覆盖是 Docker 单文件/目录挂载下的退路（容器内目录属于 root，
// 原子 rename 需要目录可写，会失败）。这里直接验证退路本身可用。
func TestWriteInPlaceOverwritesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	writeFile(t, path, `{"rules":[],"listen":"127.0.0.1:8787"}`)

	s, err := NewStore(path, nil)
	if err != nil {
		t.Fatal(err)
	}

	// 新内容比旧内容短，验证确实截断了而不是留下尾巴。
	if err := s.writeInPlace([]byte("{}\n")); err != nil {
		t.Fatalf("writeInPlace: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "{}\n" {
		t.Errorf("file = %q, want %q", string(raw), "{}\n")
	}
}

// 目录不存在时两条写回路径都走不通，必须把两个错误都报出来，
// 而不是只报原子替换那个（否则用户看不出真正原因）。
func TestSetRulesReportsBothPersistFailures(t *testing.T) {
	path := filepath.Join(t.TempDir(), "no-such-dir", "config.json")
	s, err := NewStore(filepath.Join(t.TempDir(), "seed.json"), nil)
	if err != nil {
		t.Fatal(err)
	}
	s.path = path // 指向不可写的路径：要的就是「两条写回路径都失败」这个状态

	persisted, persistErr, err := s.SetRules([]Rule{{Name: "n", Model: "m", Upstreams: []string{"a"}}})
	if err != nil {
		t.Fatalf("SetRules should not fail hard: %v", err)
	}
	if persisted || persistErr == nil {
		t.Fatalf("persisted=%v persistErr=%v, want false/<error>", persisted, persistErr)
	}
	if !strings.Contains(persistErr.Error(), "in-place fallback also failed") {
		t.Errorf("error should mention both attempts, got: %v", persistErr)
	}
	if s.Current().Rules[0].Name != "n" {
		t.Error("rules should still apply in memory when persist fails")
	}
}

// 配置里可能含 admin_token，写回时必须沿用原权限，
// 不能把运维者设的 0600 悄悄改写成 0644。
func TestSetRulesPreservesFileMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 不保留 POSIX 权限位")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	writeFile(t, path, `{"rules":[]}`)
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}

	s, err := NewStore(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	persisted, persistErr, err := s.SetRules([]Rule{{Name: "n", Model: "m", Upstreams: []string{"a"}}})
	if err != nil || !persisted || persistErr != nil {
		t.Fatalf("SetRules: persisted=%v persistErr=%v err=%v", persisted, persistErr, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("mode = %o, want 600", got)
	}
}

func TestWatchReloadsOnFileChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	writeFile(t, path, `{"rules":[{"name":"old","model":"deepseek","upstreams":["deepseek"]}]}`)

	s, err := NewStore(path, nil)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Watch(ctx, 20*time.Millisecond)

	writeFile(t, path, `{"rules":[{"name":"watched","model":"glm-5.3","upstreams":["friendli"]}]}`)

	deadline := time.After(3 * time.Second)
	for {
		if s.Current().Rules[0].Name == "watched" {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("watcher did not reload the config, still %q", s.Current().Rules[0].Name)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func TestWatchDoesNothingWithoutPath(t *testing.T) {
	s := Static(Default())
	// 不应 panic，也不应阻塞。
	done := make(chan struct{})
	go func() {
		s.Watch(context.Background(), time.Millisecond)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Watch should return immediately when there is no config file")
	}
}
