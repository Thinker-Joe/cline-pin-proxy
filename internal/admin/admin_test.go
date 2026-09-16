package admin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Thinker-Joe/cline-pin-proxy/internal/config"
	"github.com/Thinker-Joe/cline-pin-proxy/internal/probe"
)

// fakeStore 实现 RuleStore，记录调用并允许编排失败。
type fakeStore struct {
	cfg *config.Config

	setCalls   int
	setRules   []config.Rule
	persisted  bool
	persistErr error
	setErr     error

	reloadChanged bool
	reloadErr     error
}

func (f *fakeStore) Current() *config.Config { return f.cfg }

func (f *fakeStore) SetRules(rules []config.Rule) (bool, error, error) {
	f.setCalls++
	if f.setErr != nil {
		return false, nil, f.setErr
	}
	f.setRules = rules
	next := *f.cfg
	next.Rules = rules
	f.cfg = &next
	return f.persisted, f.persistErr, nil
}

func (f *fakeStore) Reload() (bool, error) { return f.reloadChanged, f.reloadErr }

type fakeProber struct {
	res probe.Result
	err error
	got []string // model, pipeline
}

func (f *fakeProber) Probe(_ context.Context, model, pipeline string) (probe.Result, error) {
	f.got = []string{model, pipeline}
	return f.res, f.err
}

func testConfig(token string) *config.Config {
	return &config.Config{
		Listen:     "127.0.0.1:8787",
		Upstream:   "https://api.cline.bot/api/v1",
		APIKey:     "sk-super-secret",
		AdminToken: token,
		Rules: []config.Rule{
			{Name: "deepseek", Model: "deepseek", Match: config.MatchContains,
				Mode: config.PinStrict, Pipeline: config.PipelineAuto, Upstreams: []string{"deepseek"}},
		},
	}
}

func newTestHandler(t *testing.T, cfg *config.Config, prober Prober) (*http.ServeMux, *fakeStore) {
	t.Helper()
	store := &fakeStore{cfg: cfg}
	mux := http.NewServeMux()
	NewHandler(store, prober, slog.New(slog.NewTextHandler(io.Discard, nil))).Register(mux)
	return mux, store
}

func do(t *testing.T, mux http.Handler, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func bearer(token string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + token}
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("response is not JSON: %v\n%s", err, rec.Body.String())
	}
	return out
}

// 未配置令牌且未显式允许匿名时，管理接口必须表现为「不存在」。
func TestDisabledByDefaultLooksLike404(t *testing.T) {
	mux, _ := newTestHandler(t, testConfig(""), &fakeProber{})
	for _, path := range []string{"/admin/config", "/admin/rules", "/admin/probe", "/admin/reload"} {
		rec := do(t, mux, http.MethodGet, path, "", nil)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s = %d, want 404（不应暴露管理接口存在）", path, rec.Code)
		}
	}
}

func TestAllowsUnauthenticatedWhenExplicitlyEnabled(t *testing.T) {
	cfg := testConfig("")
	cfg.AdminAllowUnauthenticated = true
	mux, _ := newTestHandler(t, cfg, &fakeProber{})

	rec := do(t, mux, http.MethodGet, "/admin/rules", "", nil)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestRequiresTokenWhenSet(t *testing.T) {
	mux, _ := newTestHandler(t, testConfig("s3cr3t"), &fakeProber{})

	rec := do(t, mux, http.MethodGet, "/admin/rules", "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	if got := rec.Header().Get("WWW-Authenticate"); !strings.Contains(got, "Bearer") {
		t.Errorf("WWW-Authenticate = %q", got)
	}
}

func TestAcceptsBearerAndAdminTokenHeader(t *testing.T) {
	mux, _ := newTestHandler(t, testConfig("s3cr3t"), &fakeProber{})

	for name, headers := range map[string]map[string]string{
		"bearer":     bearer("s3cr3t"),
		"x-admin":    {"X-Admin-Token": "s3cr3t"},
		"bearer raw": {"Authorization": "s3cr3t"},
	} {
		t.Run(name, func(t *testing.T) {
			rec := do(t, mux, http.MethodGet, "/admin/rules", "", headers)
			if rec.Code != http.StatusOK {
				t.Errorf("status = %d, want 200", rec.Code)
			}
		})
	}
}

func TestRejectsWrongToken(t *testing.T) {
	mux, _ := newTestHandler(t, testConfig("s3cr3t"), &fakeProber{})
	for _, tok := range []string{"wrong", "s3cr3", "s3cr3tt", ""} {
		rec := do(t, mux, http.MethodGet, "/admin/rules", "", bearer(tok))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("token %q -> %d, want 401", tok, rec.Code)
		}
	}
}

// 管理接口绝不能下发密钥。
func TestConfigEndpointDoesNotLeakSecrets(t *testing.T) {
	mux, _ := newTestHandler(t, testConfig("s3cr3t"), &fakeProber{})
	rec := do(t, mux, http.MethodGet, "/admin/config", "", bearer("s3cr3t"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "sk-super-secret") {
		t.Error("api_key leaked in /admin/config")
	}
	if strings.Contains(body, "s3cr3t") {
		t.Error("admin_token leaked in /admin/config")
	}

	out := decode(t, rec)
	if out["admin_auth_required"] != true {
		t.Errorf("admin_auth_required = %v", out["admin_auth_required"])
	}
	if out["upstream"] != "https://api.cline.bot/api/v1" {
		t.Errorf("upstream = %v", out["upstream"])
	}
}

func TestGetRules(t *testing.T) {
	mux, _ := newTestHandler(t, testConfig("t"), &fakeProber{})
	rec := do(t, mux, http.MethodGet, "/admin/rules", "", bearer("t"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	out := decode(t, rec)
	rules, ok := out["rules"].([]any)
	if !ok || len(rules) != 1 {
		t.Fatalf("rules = %#v", out["rules"])
	}
}

func TestPutRulesAppliesAndReportsPersistence(t *testing.T) {
	store := &fakeStore{cfg: testConfig("t"), persisted: true}
	mux := http.NewServeMux()
	NewHandler(store, &fakeProber{}, nil).Register(mux)

	body := `{"rules":[{"name":"glm","model":"glm-5.3","match":"contains","pipeline":"auto","mode":"strict","upstreams":["friendli"]}]}`
	rec := do(t, mux, http.MethodPut, "/admin/rules", body, bearer("t"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	out := decode(t, rec)
	if out["applied"] != true || out["persisted"] != true {
		t.Errorf("applied/persisted = %v/%v", out["applied"], out["persisted"])
	}
	if store.setCalls != 1 {
		t.Errorf("SetRules called %d times", store.setCalls)
	}
	if len(store.setRules) != 1 || store.setRules[0].Upstreams[0] != "friendli" {
		t.Errorf("rules not forwarded: %+v", store.setRules)
	}
	if _, hasErr := out["persist_error"]; hasErr {
		t.Error("no persist_error expected on success")
	}
}

// 只读挂载下写回会失败：必须仍然生效，并把原因和提示如实返回。
func TestPutRulesReportsPersistFailureButStillApplies(t *testing.T) {
	store := &fakeStore{
		cfg:        testConfig("t"),
		persisted:  false,
		persistErr: errors.New("read-only file system"),
	}
	mux := http.NewServeMux()
	NewHandler(store, &fakeProber{}, nil).Register(mux)

	body := `{"rules":[{"name":"a","model":"m","upstreams":["x"]}]}`
	rec := do(t, mux, http.MethodPut, "/admin/rules", body, bearer("t"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	out := decode(t, rec)
	if out["applied"] != true {
		t.Error("rules must still apply when persist fails")
	}
	if out["persisted"] != false {
		t.Error("persisted should be false")
	}
	if out["persist_error"] == nil || out["hint"] == nil {
		t.Errorf("expected persist_error and hint, got %v", out)
	}
}

func TestPutRulesRejectsInvalidRules(t *testing.T) {
	store := &fakeStore{cfg: testConfig("t"), setErr: errors.New("rules[0]: model must not be empty")}
	mux := http.NewServeMux()
	NewHandler(store, &fakeProber{}, nil).Register(mux)

	rec := do(t, mux, http.MethodPut, "/admin/rules", `{"rules":[]}`, bearer("t"))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestPutRulesRejectsMalformedAndUnknownFields(t *testing.T) {
	mux, _ := newTestHandler(t, testConfig("t"), &fakeProber{})

	for name, body := range map[string]string{
		"malformed":        `{oops`,
		"unknown field":    `{"rules":[],"bogus":1}`,
		"wrong field name": `{"rule":[]}`,
	} {
		t.Run(name, func(t *testing.T) {
			rec := do(t, mux, http.MethodPut, "/admin/rules", body, bearer("t"))
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", rec.Code)
			}
		})
	}
}

func TestProbeEndpoint(t *testing.T) {
	prober := &fakeProber{res: probe.Result{
		Model: "cline-pass/glm-5.3", Pipeline: "planner",
		Upstreams: []string{"friendli", "togetherai"}, Status: 500,
	}}
	mux, _ := newTestHandler(t, testConfig("t"), prober)

	rec := do(t, mux, http.MethodPost, "/admin/probe", `{"model":"cline-pass/glm-5.3"}`, bearer("t"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	out := decode(t, rec)
	if out["pipeline"] != "planner" {
		t.Errorf("pipeline = %v", out["pipeline"])
	}
	if len(prober.got) != 2 || prober.got[0] != "cline-pass/glm-5.3" {
		t.Errorf("prober got %v", prober.got)
	}
}

func TestProbeRequiresModel(t *testing.T) {
	mux, _ := newTestHandler(t, testConfig("t"), &fakeProber{})
	rec := do(t, mux, http.MethodPost, "/admin/probe", `{}`, bearer("t"))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestProbeUpstreamErrorBecomes502(t *testing.T) {
	mux, _ := newTestHandler(t, testConfig("t"), &fakeProber{err: errors.New("connection refused")})
	rec := do(t, mux, http.MethodPost, "/admin/probe", `{"model":"m"}`, bearer("t"))
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
}

func TestReloadEndpoint(t *testing.T) {
	store := &fakeStore{cfg: testConfig("t"), reloadChanged: true}
	mux := http.NewServeMux()
	NewHandler(store, &fakeProber{}, nil).Register(mux)

	rec := do(t, mux, http.MethodPost, "/admin/reload", "", bearer("t"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if out := decode(t, rec); out["changed"] != true {
		t.Errorf("changed = %v", out["changed"])
	}
}

func TestReloadFailureBecomes500(t *testing.T) {
	store := &fakeStore{cfg: testConfig("t"), reloadErr: errors.New("parse error")}
	mux := http.NewServeMux()
	NewHandler(store, &fakeProber{}, nil).Register(mux)

	rec := do(t, mux, http.MethodPost, "/admin/reload", "", bearer("t"))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
}

func TestMethodNotAllowed(t *testing.T) {
	mux, _ := newTestHandler(t, testConfig("t"), &fakeProber{})

	cases := []struct{ method, path string }{
		{http.MethodPost, "/admin/config"},
		{http.MethodPost, "/admin/rules"},
		{http.MethodGet, "/admin/probe"},
		{http.MethodGet, "/admin/reload"},
	}
	for _, c := range cases {
		rec := do(t, mux, c.method, c.path, "", bearer("t"))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s = %d, want 405", c.method, c.path, rec.Code)
		}
		if rec.Header().Get("Allow") == "" {
			t.Errorf("%s %s missing Allow header", c.method, c.path)
		}
	}
}
