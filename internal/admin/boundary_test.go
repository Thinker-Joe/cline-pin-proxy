package admin

import (
	"net/http"
	"testing"
)

// 裸 null 会解码成 nil slice，被 SetRules 当作"清空规则表"接受。
//
// 一个把未初始化变量直接序列化的脚本就能关掉全部钉死规则——而包装形式
// {"rules":null} 是被拒绝的，两种写法行为不一致。清空应当只能靠显式的 []。
func TestPutRulesRejectsBareNull(t *testing.T) {
	mux, store := newTestHandler(t, testConfig("t"), &fakeProber{})

	rec := do(t, mux, http.MethodPut, "/admin/rules", `null`, bearer("t"))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400（裸 null 不能清空规则表）", rec.Code)
	}
	if store.setCalls != 0 {
		t.Errorf("SetRules 被调用了 %d 次，非法输入不应改动任何状态", store.setCalls)
	}
	if len(store.cfg.Rules) != 1 {
		t.Errorf("规则数 = %d, want 1（保持不变）", len(store.cfg.Rules))
	}
}

// null 出现在数组元素里同样要拒绝。
func TestPutRulesRejectsNullElement(t *testing.T) {
	mux, store := newTestHandler(t, testConfig("t"), &fakeProber{})

	rec := do(t, mux, http.MethodPut, "/admin/rules", `[null]`, bearer("t"))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	if store.setCalls != 0 {
		t.Errorf("SetRules 被调用了 %d 次", store.setCalls)
	}
}

// 显式空数组仍然是合法的"关闭钉死"。
func TestPutRulesAcceptsExplicitEmptyArray(t *testing.T) {
	mux, store := newTestHandler(t, testConfig("t"), &fakeProber{})

	for _, body := range []string{`[]`, `{"rules":[]}`} {
		rec := do(t, mux, http.MethodPut, "/admin/rules", body, bearer("t"))
		if rec.Code != http.StatusOK {
			t.Fatalf("body=%s status = %d, want 200", body, rec.Code)
		}
	}
	if store.setCalls != 2 {
		t.Errorf("SetRules 被调用了 %d 次, want 2", store.setCalls)
	}
}
