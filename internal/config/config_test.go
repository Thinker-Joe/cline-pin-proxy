package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultIsValid(t *testing.T) {
	cfg := Default()
	if err := cfg.Normalize(); err != nil {
		t.Fatalf("built-in defaults must be valid: %v", err)
	}
	if cfg.Listen != DefaultListen {
		t.Errorf("Listen = %q, want %q", cfg.Listen, DefaultListen)
	}
	if cfg.Upstream != DefaultUpstream {
		t.Errorf("Upstream = %q, want %q", cfg.Upstream, DefaultUpstream)
	}
	if len(cfg.Rules) == 0 {
		t.Fatal("default rules should cover deepseek and glm")
	}
	// 默认必须带上 DeepSeek 与 GLM 两条，这是本项目要解决的核心场景。
	if _, ok := cfg.Match("cline-pass/deepseek-v4.1-flash"); !ok {
		t.Error("default rules should match deepseek models")
	}
	if _, ok := cfg.Match("cline-pass/glm-5.3"); !ok {
		t.Error("default rules should match glm models")
	}
}

func TestMatchModes(t *testing.T) {
	cases := []struct {
		name  string
		match MatchMode
		key   string
		model string
		want  bool
	}{
		{"contains hits path prefix segment", MatchContains, "deepseek", "cline-pass/deepseek-v4.1-flash", true},
		{"contains is case insensitive", MatchContains, "DeepSeek", "cline-pass/DeepSeek-V4", true},
		{"contains miss", MatchContains, "deepseek", "cline-pass/glm-5.3", false},
		{"prefix hits", MatchPrefix, "cline-pass/", "cline-pass/glm-5.3", true},
		{"prefix miss when not at start", MatchPrefix, "glm", "cline-pass/glm-5.3", false},
		{"exact hits", MatchExact, "cline-pass/glm-5.3", "cline-pass/glm-5.3", true},
		{"exact miss on suffix", MatchExact, "glm-5.3", "cline-pass/glm-5.3", false},
		{"empty key never matches", MatchContains, "", "cline-pass/glm-5.3", false},
		{"empty model never matches", MatchContains, "glm", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := Rule{Model: tc.key, Match: tc.match}
			if got := r.Matches(tc.model); got != tc.want {
				t.Errorf("Matches(%q) = %v, want %v", tc.model, got, tc.want)
			}
		})
	}
}

// 规则按数组顺序生效，首个命中者胜出。
func TestMatchFirstRuleWins(t *testing.T) {
	cfg := &Config{Rules: []Rule{
		{Name: "specific", Model: "cline-pass/deepseek-v4.1-flash", Match: MatchExact, Upstreams: []string{"deepseek"}, Mode: PinStrict},
		{Name: "broad", Model: "deepseek", Match: MatchContains, Upstreams: []string{"other"}, Mode: PinStrict},
	}}
	got, ok := cfg.Match("cline-pass/deepseek-v4.1-flash")
	if !ok || got.Name != "specific" {
		t.Fatalf("Match = %+v (ok=%v), want the exact rule first", got, ok)
	}
}

func TestMatchOnNilOrEmpty(t *testing.T) {
	var nilCfg *Config
	if _, ok := nilCfg.Match("anything"); ok {
		t.Error("nil config must not match")
	}
	empty := &Config{}
	if _, ok := empty.Match("anything"); ok {
		t.Error("empty rules must not match")
	}
}

func TestNormalizeRejectsInvalidRules(t *testing.T) {
	cases := []struct {
		name string
		rule Rule
		want string
	}{
		{"empty model", Rule{Upstreams: []string{"a"}}, "model must not be empty"},
		{"bad match", Rule{Model: "m", Match: "regex", Upstreams: []string{"a"}}, "match must be one of"},
		{"bad pipeline", Rule{Model: "m", Pipeline: "sideways", Upstreams: []string{"a"}}, "pipeline must be one of"},
		{"bad mode", Rule{Model: "m", Mode: "loose", Upstreams: []string{"a"}}, "mode must be one of"},
		{"no upstreams", Rule{Model: "m"}, "upstreams must not be empty"},
		{"preferred with one upstream", Rule{Model: "m", Mode: PinPreferred, Upstreams: []string{"a"}}, "needs at least 2 upstreams"},
		{"bad sort", Rule{Model: "m", Upstreams: []string{"a"}, Sort: "colour"}, "sort must be one of"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.rule.Normalize(0)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}

// 缺省字段要按文档补齐，而不是留空导致运行期行为不确定。
func TestNormalizeFillsDefaults(t *testing.T) {
	r := Rule{Model: "m", Upstreams: []string{" a ", "", "b"}}
	if err := r.Normalize(0); err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if r.Match != MatchContains {
		t.Errorf("Match default = %q", r.Match)
	}
	if r.Pipeline != PipelineAuto {
		t.Errorf("Pipeline default = %q", r.Pipeline)
	}
	if r.Mode != PinStrict {
		t.Errorf("Mode default = %q", r.Mode)
	}
	if r.Name == "" {
		t.Error("Name should get a placeholder")
	}
	if len(r.Upstreams) != 2 || r.Upstreams[0] != "a" || r.Upstreams[1] != "b" {
		t.Errorf("Upstreams not trimmed/deduped of blanks: %v", r.Upstreams)
	}
}

func TestNormalizeSortNoneBecomesEmpty(t *testing.T) {
	for _, in := range []string{"none", "NONE", "  "} {
		r := Rule{Model: "m", Upstreams: []string{"a"}, Sort: in}
		if err := r.Normalize(0); err != nil {
			t.Fatalf("Normalize(%q): %v", in, err)
		}
		if r.Sort != "" {
			t.Errorf("Sort %q should normalize to empty, got %q", in, r.Sort)
		}
	}
}

func TestNormalizeRejectsBadUpstream(t *testing.T) {
	cfg := Default()
	cfg.Upstream = "api.cline.bot/api/v1"
	if err := cfg.Normalize(); err == nil {
		t.Fatal("upstream without scheme must be rejected")
	}
}

func TestNormalizeDedupesForwardHeaders(t *testing.T) {
	cfg := Default()
	cfg.ForwardHeaders = []string{"X-Client-Type", "x-client-type", " ", "X-App"}
	if err := cfg.Normalize(); err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if len(cfg.ForwardHeaders) != 2 {
		t.Fatalf("ForwardHeaders = %v, want 2 unique entries", cfg.ForwardHeaders)
	}
	for _, h := range cfg.ForwardHeaders {
		if h != strings.ToLower(h) {
			t.Errorf("header %q should be lowercased", h)
		}
	}
}

func TestLoadFromFileOverridesOnlyGivenFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	content := `{"listen":"0.0.0.0:9000","rules":[{"name":"only-ds","model":"deepseek","upstreams":["deepseek"],"mode":"strict"}]}`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Listen != "0.0.0.0:9000" {
		t.Errorf("Listen = %q", cfg.Listen)
	}
	// 文件里没写的字段应保留默认值。
	if cfg.Upstream != DefaultUpstream {
		t.Errorf("Upstream should keep default, got %q", cfg.Upstream)
	}
	if cfg.MaxBodyBytes != DefaultMaxBodyBytes {
		t.Errorf("MaxBodyBytes should keep default, got %d", cfg.MaxBodyBytes)
	}
	if len(cfg.Rules) != 1 || cfg.Rules[0].Name != "only-ds" {
		t.Errorf("Rules = %+v", cfg.Rules)
	}
	// 规则里没写的字段也应补齐。
	if cfg.Rules[0].Pipeline != PipelineAuto || cfg.Rules[0].Match != MatchContains {
		t.Errorf("rule defaults not applied: %+v", cfg.Rules[0])
	}
}

func TestLoadMissingFileFails(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope.json")); err == nil {
		t.Fatal("expected error for missing config file")
	}
}

func TestLoadInvalidJSONFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(path, []byte(`{oops`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected error for malformed config")
	}
}

func TestLoadWithNoPathUsesDefaults(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load(\"\"): %v", err)
	}
	if cfg.Listen != DefaultListen {
		t.Errorf("Listen = %q", cfg.Listen)
	}
}

func TestEnvOverrides(t *testing.T) {
	t.Setenv("CLINE_PIN_LISTEN", "0.0.0.0:1234")
	t.Setenv("CLINE_PIN_UPSTREAM", "https://example.test/api/v1/")
	t.Setenv("CLINE_PIN_API_KEY", "sk-test")
	t.Setenv("CLINE_PIN_MAX_BODY_BYTES", "1024")
	t.Setenv("CLINE_PIN_FORWARD_HEADERS", "x-client-type, x-app")
	t.Setenv("CLINE_PIN_RULES", `[{"name":"env","model":"deepseek","upstreams":["deepseek"]}]`)

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Listen != "0.0.0.0:1234" {
		t.Errorf("Listen = %q", cfg.Listen)
	}
	if cfg.Upstream != "https://example.test/api/v1" {
		t.Errorf("Upstream trailing slash should be trimmed, got %q", cfg.Upstream)
	}
	if cfg.APIKey != "sk-test" {
		t.Errorf("APIKey = %q", cfg.APIKey)
	}
	if cfg.MaxBodyBytes != 1024 {
		t.Errorf("MaxBodyBytes = %d", cfg.MaxBodyBytes)
	}
	if len(cfg.ForwardHeaders) != 2 {
		t.Errorf("ForwardHeaders = %v", cfg.ForwardHeaders)
	}
	if len(cfg.Rules) != 1 || cfg.Rules[0].Name != "env" {
		t.Errorf("Rules = %+v", cfg.Rules)
	}
}

func TestEnvIgnoresInvalidValues(t *testing.T) {
	t.Setenv("CLINE_PIN_MAX_BODY_BYTES", "not-a-number")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.MaxBodyBytes != DefaultMaxBodyBytes {
		t.Errorf("invalid env value should be ignored, got %d", cfg.MaxBodyBytes)
	}
}
