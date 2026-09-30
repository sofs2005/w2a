package server

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/session"
)

// 别名映射的端到端行为（统一调度改造 阶段2）：
//   - 出站 model 按**选中账号的域**改写（同一对外名在两域可映射到不同物理名）；
//   - 单域别名收紧候选域（另一域账号根本不参与选号，不吃一次 11102）；
//   - 单域别名被显式钉到不存在的那一域 → 400（不浪费轮转）；
//   - 粘性会话跨域保持（裸名不再把 global 号判为"域不符"而解绑）。

// TestChatAliasRewritesOutboundPerRealm 双域别名：同一对外名按选中账号域改写为
// 各自物理名（CN 号 → cn 名，global 号 → global 名）。
func TestChatAliasRewritesOutboundPerRealm(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	cf := newRealmFake(t)
	store := aliasStoreForTest(t, `{"aliases":[{"name":"my-model","cn":"cn-phys","global":"gl-phys"}]}`)

	// 池里只有 CN 号 → 必选 CN，出站名取条目 cn 侧。
	pCN := testPoolWith(&auth.Auth{UID: "cn1", AccessToken: "at_cn", Domain: "www.codebuddy.cn", ExpiresAt: 9999999999})
	hCN := NewHandler(Config{Pool: pCN, Upstream: cf.up, GlobalEnabled: true, Aliases: store})
	rec := httptest.NewRecorder()
	hCN.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"my-model","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != 200 {
		t.Fatalf("cn alias chat code=%d body=%s", rec.Code, rec.Body)
	}
	cf.mu.Lock()
	gotAuthz, gotModel := cf.authz, cf.model
	cf.mu.Unlock()
	if gotAuthz != "Bearer at_cn" {
		t.Errorf("cn alias authz=%q want Bearer at_cn", gotAuthz)
	}
	if gotModel != "cn-phys" {
		t.Errorf("cn alias outbound model=%q want cn-phys", gotModel)
	}

	// 池里只有 global 号 → 同一对外名走另一条映射（出站名按账号域改写）。
	pGL := testPoolWith(&auth.Auth{UID: "g1", AccessToken: "at_gl", Domain: "www.workbuddy.ai", ExpiresAt: 9999999999})
	hGL := NewHandler(Config{Pool: pGL, Upstream: cf.up, GlobalEnabled: true, Aliases: store})
	rec = httptest.NewRecorder()
	hGL.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"my-model","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != 200 {
		t.Fatalf("global alias chat code=%d body=%s", rec.Code, rec.Body)
	}
	cf.mu.Lock()
	gotAuthz, gotModel = cf.authz, cf.model
	cf.mu.Unlock()
	if gotAuthz != "Bearer at_gl" {
		t.Errorf("global alias authz=%q want Bearer at_gl", gotAuthz)
	}
	if gotModel != "gl-phys" {
		t.Errorf("global alias outbound model=%q want gl-phys", gotModel)
	}
}

// TestChatAliasSingleDomainExcludesOtherRealm 单域别名（只有 global 名）收紧候选域：
// 池里 CN 号权重/UID 序都排在前（无收紧时必选 cn1），收紧后必须只选 global 号
// ——避免选中后吃一次 11102 再换号。
func TestChatAliasSingleDomainExcludesOtherRealm(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	cf := newRealmFake(t)
	store := aliasStoreForTest(t, `{"aliases":[{"name":"hidden-x","global":"gl-only"}]}`)
	p := testPoolWith(
		&auth.Auth{UID: "cn1", AccessToken: "at_cn", Domain: "www.codebuddy.cn", ExpiresAt: 9999999999},
		&auth.Auth{UID: "g1", AccessToken: "at_gl", Domain: "www.workbuddy.ai", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: cf.up, GlobalEnabled: true, Aliases: store})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"hidden-x","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != 200 {
		t.Fatalf("single-domain alias chat code=%d body=%s", rec.Code, rec.Body)
	}
	cf.mu.Lock()
	gotAuthz, gotModel := cf.authz, cf.model
	cf.mu.Unlock()
	if gotAuthz != "Bearer at_gl" {
		t.Errorf("单域（global）别名不得选中 CN 号：authz=%q want Bearer at_gl", gotAuthz)
	}
	if gotModel != "gl-only" {
		t.Errorf("outbound model=%q want gl-only", gotModel)
	}
}

// TestChatAliasPinnedToMissingDomain400 单域别名被前缀显式钉到不存在的那一域：
// 无账号能承接 → 400 model_not_available，不进入轮转。
func TestChatAliasPinnedToMissingDomain400(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	cf := newRealmFake(t)
	store := aliasStoreForTest(t, `{"aliases":[{"name":"hidden-x","global":"gl-only"}]}`)
	p := testPoolWith(
		&auth.Auth{UID: "cn1", AccessToken: "at_cn", Domain: "www.codebuddy.cn", ExpiresAt: 9999999999},
		&auth.Auth{UID: "g1", AccessToken: "at_gl", Domain: "www.workbuddy.ai", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: cf.up, GlobalEnabled: true, Aliases: store})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"cn:hidden-x","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != 400 {
		t.Fatalf("cn:hidden-x code=%d want 400 body=%s", rec.Code, rec.Body)
	}
	if !assertJSONErrorCode(t, rec.Body.String(), "model_not_available") {
		t.Errorf("cn:hidden-x error code want model_not_available: %s", rec.Body)
	}
}

// TestChatAliasStickyAcrossRealms 裸名（统一调度）下粘性会话钉在 global 号上仍然有效：
// 改造前裸名被解析为 cn，realm 校验会把 global 粘性号判为"域不符"而解绑；改造后
// 裸名是全池，粘性保持，出站名按 global 侧改写。
func TestChatAliasStickyAcrossRealms(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	cf := newRealmFake(t)
	st := newBindStore()
	sess := session.New(session.Config{
		TTL:       time.Minute,
		Store:     st,
		Available: func() []string { return []string{"cn1", "g1"} },
	})
	store := aliasStoreForTest(t, `{"aliases":[{"name":"my-model","cn":"cn-phys","global":"gl-phys"}]}`)
	p := testPoolWith(
		&auth.Auth{UID: "cn1", AccessToken: "at_cn", Domain: "www.codebuddy.cn", ExpiresAt: 9999999999},
		&auth.Auth{UID: "g1", AccessToken: "at_gl", Domain: "www.workbuddy.ai", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: cf.up, Session: sess, GlobalEnabled: true, Aliases: store})

	sess.Bind("conv-1", "g1") // 会话历史绑定在 global 号上
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"my-model","messages":[],"metadata":{"conversation_id":"conv-1"}}`)))
	if rec.Code != 200 {
		t.Fatalf("sticky alias chat code=%d body=%s", rec.Code, rec.Body)
	}
	cf.mu.Lock()
	gotAuthz, gotModel := cf.authz, cf.model
	cf.mu.Unlock()
	if gotAuthz != "Bearer at_gl" {
		t.Errorf("粘性号应为 global 号（裸名不再判域不符）：authz=%q want Bearer at_gl", gotAuthz)
	}
	if gotModel != "gl-phys" {
		t.Errorf("粘性请求出站 model=%q want gl-phys（按 global 域改写）", gotModel)
	}
	if uid, ok := st.lastUID("conv-1"); !ok || uid != "g1" {
		t.Errorf("粘性绑定不得被解绑，got uid=%q ok=%v binds=%v", uid, ok, st.binds)
	}
}
