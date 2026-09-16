package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CLI 此前完全没有测试。这里覆盖最容易出事、也最常被运维直接调用的几条路径：
// 子命令分发，以及 check 对**非法配置**必须返回失败。
//
// 尤其是 `check`：它的价值全在"配错了要拦住"。如果非法配置也能通过校验，
// 运维会带着一个坏配置去重启容器。

func TestRunRejectsUnknownSubcommand(t *testing.T) {
	if err := run([]string{"frobnicate"}); err == nil {
		t.Fatal("未知子命令必须返回错误")
	}
}

func TestRunVersionAndHelpSucceed(t *testing.T) {
	for _, sub := range []string{"version", "help", "-h", "--help"} {
		if err := run([]string{sub}); err != nil {
			t.Errorf("run(%q) = %v, want nil", sub, err)
		}
	}
}

// check 在配置合法时必须通过。
func TestCheckAcceptsValidConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	body := `{"rules":[{"name":"a","model":"glm-5.3","upstreams":["friendli"]}]}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"check", "-config", path, "-model", "cline-pass/glm-5.3"}); err != nil {
		t.Errorf("check 应通过: %v", err)
	}
}

// check 在配置非法时必须失败——这是它存在的意义。
func TestCheckFailsOnInvalidConfig(t *testing.T) {
	cases := map[string]string{
		"坏 JSON":           `{oops`,
		"顶层 null":          `null`,
		"缺 upstreams":      `{"rules":[{"model":"custom"}]}`,
		"空 model":          `{"rules":[{"upstreams":["x"]}]}`,
		"preferred 只有一个候选": `{"rules":[{"model":"m","mode":"preferred","upstreams":["x"]}]}`,
		"非法 upstream":      `{"upstream":"http://%"}`,
		"未知 match":         `{"rules":[{"model":"m","match":"fuzzy","upstreams":["x"]}]}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.json")
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := run([]string{"check", "-config", path}); err == nil {
				t.Errorf("非法配置 %s 必须让 check 失败", body)
			}
		})
	}
}

// 文件不存在时 check 应当报错，而不是静默用默认规则通过。
func TestCheckFailsWhenConfigFileMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nope.json")
	if err := run([]string{"check", "-config", path}); err == nil {
		t.Error("配置文件不存在时 check 必须失败")
	}
}

// 非法环境变量也不能绕过 check。
func TestCheckFailsOnInvalidEnvRules(t *testing.T) {
	t.Setenv("CLINE_PIN_RULES", `[{invalid`)
	if err := run([]string{"check"}); err == nil {
		t.Error("非法的 CLINE_PIN_RULES 必须让 check 失败")
	}
}

// healthcheck 指向上不存在的地址时必须返回错误（容器 HEALTHCHECK 依赖它）。
func TestHealthcheckFailsOnUnreachableTarget(t *testing.T) {
	err := run([]string{"healthcheck", "-url", "http://127.0.0.1:1/healthz"})
	if err == nil {
		t.Fatal("不可达的 /healthz 必须返回错误")
	}
	if !strings.Contains(err.Error(), "health") && !strings.Contains(err.Error(), "unhealthy") {
		t.Logf("错误信息: %v", err)
	}
}
