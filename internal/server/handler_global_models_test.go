package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"workbuddy2api/internal/aliases"
	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

// globalModelsHandlerFake 探测 fake：记录模型目录 GET 请求次数/路径/鉴权头，并可控响应。
// 只服务于模型目录端点；chat 路径一律 404（本测试不触发 chat）。
type globalModelsHandlerFake struct {
	up *upstream.Client

	mu   sync.Mutex
	cnt  int    // 模型目录探测请求计数（含 console 与 /v2 家族）
	path string // 最近一次探测路径
	auth string // 最近一次探测鉴权头
	host string // 最近一次探测 Host

	status int
	body   string
	// cnBody 覆盖 CN 动态拉取响应（空 = 默认的 cn-dyn-model 单模型表）。
	// 供并集去重测试构造"同一模型两域都有"的目录。
	cnBody string
}

func newGlobalModelsHandlerFake(t *testing.T, status int, body string) *globalModelsHandlerFake {
	t.Helper()
	cf := &globalModelsHandlerFake{up: &upstream.Client{}, status: status, body: body}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		isCN := r.Header.Get("Authorization") == "Bearer at_cn"
		cf.mu.Lock()
		if !isCN {
			// cnt/path/auth/host 只记 global 探测请求；CN 动态拉取（FetchModels，
			// Bearer at_cn）不计——纯动态后 CN 面也需要 fake 数据源，但不能污染
			// global 探测计数断言（本文件所有 CN 号均为 at_cn）。
			cf.cnt++
			cf.path = r.URL.Path
			cf.auth = r.Header.Get("Authorization")
			cf.host = r.Host
		}
		cf.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if isCN {
			// CN 动态拉取：返回含 cli agent 的动态模型表，让 CN 面在纯动态下有产出
			//（无静态兜底后需要真实数据源）。console 路径与 global 探测家族的
			// /console fallback 同名，故按鉴权头而非路径分流。
			cnBody := cf.cnBody
			if cnBody == "" {
				cnBody = `{"code":0,"data":{"models":[
				{"id":"cn-dyn-model","maxInputTokens":65536,"maxOutputTokens":8192}
			],"agents":[{"name":"cli","models":["cn-dyn-model"]}]}}`
			}
			w.WriteHeader(200)
			_, _ = io.WriteString(w, cnBody)
			return
		}
		w.WriteHeader(cf.status)
		_, _ = io.WriteString(w, cf.body)
	}))
	cf.up = &upstream.Client{
		HTTP:           &http.Client{},
		ChatBaseCN:     strings.TrimSuffix(ts.URL, "/"), // CN 动态拉取同 fake（鉴权头分流）
		ChatBaseGlobal: strings.TrimSuffix(ts.URL, "/"),
		GlobalEnabled:  true,
	}
	return cf
}

func (cf *globalModelsHandlerFake) snapshot() (cnt int, path, auth, host string) {
	cf.mu.Lock()
	defer cf.mu.Unlock()
	return cf.cnt, cf.path, cf.auth, cf.host
}

// modelsProbeBody 构造探测端点对象数组响应。
func modelsProbeBody(ids ...string) string {
	var b strings.Builder
	b.WriteString(`{"code":0,"data":{"models":[`)
	for i, id := range ids {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"id":"` + id + `","name":"` + id + `"}`)
	}
	b.WriteString(`]}}`)
	return b.String()
}

// TestModelListTwoFamilies 断言 /v1/models 是两域**裸名并集**：CN 动态目录与
// global 探测目录同表输出、同名去重、无前缀条目、realms 标注各自可用域；
// global 名单 = 纯探测结果（不合并静态）；探测走 global base（httptest host）。
func TestModelListTwoFamilies(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	resetModelsCache()

	cf := newGlobalModelsHandlerFake(t, 200, modelsProbeBody("gpt-5.4", "probe-only-x"))
	p := testPoolWith(
		&auth.Auth{UID: "cn1", AccessToken: "at_cn", Domain: "www.codebuddy.cn", ExpiresAt: 9999999999},
		&auth.Auth{UID: "g1", AccessToken: "at_gl", Domain: "www.workbuddy.ai", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: cf.up, GlobalEnabled: true})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if rec.Code != 200 {
		t.Fatalf("GET /v1/models code=%d", rec.Code)
	}
	var resp struct {
		Data []map[string]any `json:"data"`
	}
	if err := jsonUnmarshal(rec.Body.String(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// 并集目录：id 一律裸名（无 cn:/global: 前缀条目），两域条目同表。
	allIDs := make([]string, 0, len(resp.Data))
	realmsOf := map[string][]string{}
	for _, m := range resp.Data {
		id, _ := m["id"].(string)
		if strings.HasPrefix(id, "cn:") || strings.HasPrefix(id, "global:") {
			t.Errorf("目录不得再有前缀条目: %q", id)
			continue
		}
		allIDs = append(allIDs, id)
		if rs, ok := m["realms"].([]any); ok {
			for _, r := range rs {
				if s, ok := r.(string); ok {
					realmsOf[id] = append(realmsOf[id], s)
				}
			}
		}
	}
	if !contains(allIDs, "cn-dyn-model") {
		t.Errorf("no CN entries in /v1/models: %v", allIDs)
	}
	if !contains(allIDs, "probe-only-x") {
		t.Fatal("no global entries in /v1/models")
	}
	// global 名单 = 纯探测结果：探测独有在下发名单内；静态历史名单成员不出现。
	if contains(allIDs, "default-model") || contains(allIDs, "kimi-k2.6") {
		t.Errorf("global models must not contain unprobed static names: %v", allIDs)
	}
	if countOf(allIDs, "gpt-5.4") != 1 {
		t.Errorf("union dedupe failed: gpt-5.4 count=%d", countOf(allIDs, "gpt-5.4"))
	}
	// realms 标注：探测独有的模型只属于 global；CN 动态独有的只属于 cn。
	if got := realmsOf["probe-only-x"]; len(got) != 1 || got[0] != "global" {
		t.Errorf("probe-only-x realms=%v want [global]", got)
	}
	if got := realmsOf["cn-dyn-model"]; len(got) != 1 || got[0] != "cn" {
		t.Errorf("cn-dyn-model realms=%v want [cn]", got)
	}

	// 探测走 global base（httptest Host）+ Bearer 鉴权头；v3-config-merge 后单次探测
	// = /v3/config + /v2 企业路并发（cnt=2，path 记录的是最近一次——两路之一）。
	cnt, path, authz, host := cf.snapshot()
	if cnt != 2 {
		t.Errorf("probe cnt=%d want 2 (v3/config + v2 enterprise, concurrent)", cnt)
	}
	if path != "/v2/enterprises/personal/models" && path != "/v3/config" {
		t.Errorf("probe path=%q want one of [/v2/enterprises/personal/models /v3/config]", path)
	}
	if authz != "Bearer at_gl" {
		t.Errorf("probe authz=%q want Bearer at_gl", authz)
	}
	if host == "" || host == "fake.example" {
		t.Errorf("probe host=%q want global base (httptest)", host)
	}
	// CN 动态拉取走同一 fake（ChatBaseCN 同 host，console 路径分流返回动态模型表），
	// cn-dyn-model 已在上面 allIDs 断言覆盖——不断言 CN 请求计数，避免耦合 CN 缓存重置时序。
}

// TestModelListNoGlobalAccountEmpty 无 global 账号：global 名单为空（纯动态，无静态兜底），探测零调用。
func TestModelListNoGlobalAccountEmpty(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	resetModelsCache()

	cf := newGlobalModelsHandlerFake(t, 200, modelsProbeBody("gpt-5.4"))
	p := testPoolWith(
		&auth.Auth{UID: "cn1", AccessToken: "at_cn", Domain: "www.codebuddy.cn", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: cf.up, GlobalEnabled: true})

	got := h.modelList()
	for _, m := range got {
		if rs, ok := m["realms"].([]string); ok && len(rs) == 1 && rs[0] == "global" {
			t.Fatalf("no-global-account: global entry %v want none (pure dynamic)", m["id"])
		}
	}
	cnt, _, _, _ := cf.snapshot()
	if cnt != 0 {
		t.Errorf("no-global-account: probe calls=%d want 0 (zero upstream calls)", cnt)
	}
}

// TestModelListProbeFailureEmpty 探测失败（家族全 500）→ global 名单为空（无静态回落）。
func TestModelListProbeFailureEmpty(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	resetModelsCache()

	cf := newGlobalModelsHandlerFake(t, 500, `{"code":500,"msg":"boom"}`)
	p := testPoolWith(
		&auth.Auth{UID: "g1", AccessToken: "at_gl", Domain: "www.workbuddy.ai", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: cf.up, GlobalEnabled: true})

	got := h.modelList()
	for _, m := range got {
		if rs, ok := m["realms"].([]string); ok && len(rs) == 1 && rs[0] == "global" {
			t.Errorf("probe-failure: global entry %v want none (no static fallback)", m["id"])
		}
	}
}

// TestModelListProbeCacheWithinTTL 首次探测成功 → 同 Client 二次 /v1/models 不重复探测（零新上游请求）。
func TestModelListProbeCacheWithinTTL(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	resetModelsCache()

	cf := newGlobalModelsHandlerFake(t, 200, modelsProbeBody("gpt-5.4", "probe-only-x"))
	p := testPoolWith(
		&auth.Auth{UID: "g1", AccessToken: "at_gl", Domain: "www.workbuddy.ai", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: cf.up, GlobalEnabled: true})

	h.modelList()
	cnt1, _, _, _ := cf.snapshot()
	// v3-config-merge：单次探测 = v3/config + /v2 企业路并发 = 2 个请求。
	if cnt1 != 2 {
		t.Fatalf("first probe calls=%d want 2 (v3 + v2, concurrent)", cnt1)
	}
	h.modelList()
	cnt2, _, _, _ := cf.snapshot()
	if cnt2 != cnt1 {
		t.Errorf("cache: second modelList probe calls=%d want %d (hit 1h cache)", cnt2, cnt1)
	}
}

// jsonUnmarshal 单一用途反序列化（httptest body 而非直接 resp）。
func jsonUnmarshal(s string, v any) error {
	return json.Unmarshal([]byte(s), v)
}

// contains 名单成员判定（顺序无关）。
func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// aliasStoreForTest 由 JSON 字面量构造别名 Store（解析失败直接 Fatal）。
func aliasStoreForTest(t *testing.T, raw string) *aliases.Store {
	t.Helper()
	tbl, err := aliases.Parse([]byte(raw))
	if err != nil {
		t.Fatalf("解析测试别名表: %v", err)
	}
	s := aliases.NewStore()
	s.Set(tbl)
	return s
}

// catalogRows 把目录输出索引成 id → 条目。
func catalogRows(t *testing.T, h *Handler) map[string]map[string]any {
	t.Helper()
	byID := map[string]map[string]any{}
	for _, m := range h.modelList() {
		id, ok := m["id"].(string)
		if !ok {
			t.Fatalf("目录条目无 id: %v", m)
		}
		byID[id] = m
	}
	return byID
}

// TestModelListUnionDedupAndRealms 并集去重：同名模型两域都有 → 只出一条，
// CN 富字段优先（credits 不被 global 空值抹掉），realms 标注 [cn global]；
// 单域独有条目 realms 只含所属域。
func TestModelListUnionDedupAndRealms(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	resetModelsCache()

	cf := newGlobalModelsHandlerFake(t, 200, `{"code":0,"data":{"models":[
		{"id":"glm-5.2"},
		{"id":"gpt-5.4"}
	],"agents":[{"name":"cli","models":["glm-5.2","gpt-5.4"]}]}}`)
	// CN 侧同名模型带富字段（credits/name），global 侧只有裸 id。
	cf.cnBody = `{"code":0,"data":{"models":[
		{"id":"glm-5.2","name":"GLM 5.2","credits":"x0.06","maxInputTokens":200000,"maxOutputTokens":131072},
		{"id":"cn-only-x","maxInputTokens":65536,"maxOutputTokens":8192}
	],"agents":[{"name":"cli","models":["glm-5.2","cn-only-x"]}]}}`
	p := testPoolWith(
		&auth.Auth{UID: "cn1", AccessToken: "at_cn", Domain: "www.codebuddy.cn", ExpiresAt: 9999999999},
		&auth.Auth{UID: "g1", AccessToken: "at_gl", Domain: "www.workbuddy.ai", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: cf.up, GlobalEnabled: true})

	byID := catalogRows(t, h)
	// 同名去重：glm-5.2 只出一条。
	if _, ok := byID["glm-5.2"]; !ok {
		t.Fatalf("缺 glm-5.2: %v", byID)
	}
	if got := realmsOf(t, byID["glm-5.2"]); !reflect.DeepEqual(got, []string{"cn", "global"}) {
		t.Errorf("glm-5.2 realms=%v want [cn global]", got)
	}
	// CN 富字段优先：global 裸条目不得覆盖 CN 的 credits/name。
	if byID["glm-5.2"]["credits"] != "x0.06" {
		t.Errorf("glm-5.2 credits=%v want x0.06（CN 优先，global 不覆盖）", byID["glm-5.2"]["credits"])
	}
	if byID["glm-5.2"]["name"] != "GLM 5.2" {
		t.Errorf("glm-5.2 name=%v want GLM 5.2", byID["glm-5.2"]["name"])
	}
	// 单域独有条目：realms 只含所属域。
	if got := realmsOf(t, byID["gpt-5.4"]); !reflect.DeepEqual(got, []string{"global"}) {
		t.Errorf("gpt-5.4 realms=%v want [global]", got)
	}
	if got := realmsOf(t, byID["cn-only-x"]); !reflect.DeepEqual(got, []string{"cn"}) {
		t.Errorf("cn-only-x realms=%v want [cn]", got)
	}
	// 目录不得残留前缀条目（并集不靠加前缀去重）。
	for id := range byID {
		if strings.HasPrefix(id, "cn:") || strings.HasPrefix(id, "global:") {
			t.Errorf("目录残留前缀条目: %q", id)
		}
	}
}

// TestModelListAliasEntries 别名条目进目录：隐藏模型（目录里不存在）按对外名出现，
// 带 aliased 标记；realms 按映射非空侧标注（单域别名只标该域）。
func TestModelListAliasEntries(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	resetModelsCache()

	cf := newGlobalModelsHandlerFake(t, 200, modelsProbeBody("glm-5.2"))
	p := testPoolWith(
		&auth.Auth{UID: "cn1", AccessToken: "at_cn", Domain: "www.codebuddy.cn", ExpiresAt: 9999999999},
		&auth.Auth{UID: "g1", AccessToken: "at_gl", Domain: "www.workbuddy.ai", ExpiresAt: 9999999999},
	)
	store := aliasStoreForTest(t, `{"aliases":[
		{"name":"my-hidden","global":"deepseek:hidden-v9"},
		{"name":"my-dual","cn":"cn-secret-a","global":"gl-secret-b"},
		{"name":"glm-5.2","cn":"glm-5.2","global":"glm-5.2"}
	]}`)
	h := NewHandler(Config{Pool: p, Upstream: cf.up, GlobalEnabled: true, Aliases: store})

	byID := catalogRows(t, h)
	// 隐藏模型按对外名进目录，带 aliased 标记 + 域标注（单域别名只标 global）。
	hid, ok := byID["my-hidden"]
	if !ok {
		t.Fatalf("别名条目 my-hidden 未进目录: %v", byID)
	}
	if hid["aliased"] != true {
		t.Errorf("my-hidden aliased=%v want true", hid["aliased"])
	}
	if got := realmsOf(t, hid); !reflect.DeepEqual(got, []string{"global"}) {
		t.Errorf("my-hidden realms=%v want [global]", got)
	}
	// 双域别名：realms 两域都标。
	if got := realmsOf(t, byID["my-dual"]); !reflect.DeepEqual(got, []string{"cn", "global"}) {
		t.Errorf("my-dual realms=%v want [cn global]", got)
	}
	// 别名名与目录既有条目同名 → 只并入域标注，不重复输出、不标 aliased
	//（它本来就是真实模型，别名的存在不改变其元数据）。
	if _, ok := byID["glm-5.2"]; !ok {
		t.Fatal("glm-5.2 应仍在目录中")
	}
	if byID["glm-5.2"]["aliased"] != nil {
		t.Errorf("glm-5.2 不得标 aliased（目录既有条目）: %v", byID["glm-5.2"]["aliased"])
	}
	if got := realmsOf(t, byID["glm-5.2"]); !reflect.DeepEqual(got, []string{"cn", "global"}) {
		t.Errorf("glm-5.2 realms=%v want [cn global]（别名并入域标注）", got)
	}
}

// realmsOf 取条目的 realms 列表。
func realmsOf(t *testing.T, m map[string]any) []string {
	t.Helper()
	rs, ok := m["realms"].([]string)
	if !ok {
		return nil
	}
	return rs
}

func countOf(list []string, s string) int {
	n := 0
	for _, v := range list {
		if v == s {
			n++
		}
	}
	return n
}
