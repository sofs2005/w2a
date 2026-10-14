// v3_ua_routes_test.go /v3/config 三路 UA 探测（桌面端主路 + IDE + CLI）的
// 合并语义与 UA 隔离验证。吸收 fork linguo2625469 7b24e22 的 UA 并集部分。
package upstream

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"workbuddy2api/internal/auth"
)

// methodPathUA uaRoutedSrv 记录的调用条目格式：`<path>|<UA>`。
func methodPathUA(path, ua string) string { return path + "|" + ua }

// uaRoutedSrv 按 User-Agent 路由的探测 fake：三条 v3 路各自返回不同模型集合，
// 记录每次请求的 (path, UA)。用于验证「同一次探测按 UA 发多路 + 按权威序合并」。
func uaRoutedSrv(t *testing.T, byUA map[string]string, calls *[]string) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		*calls = append(*calls, r.URL.Path+"|"+r.Header.Get("User-Agent"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/v3/config" {
			// 企业端点家族返回空目录（本组用例只关心 v3 三路的合并语义）。
			w.WriteHeader(200)
			_, _ = w.Write([]byte(modelsResp("enterprise-only")))
			return
		}
		body, ok := byUA[r.Header.Get("User-Agent")]
		if !ok {
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"code":12403,"msg":"check ua, get coding copilot version error"}`))
			return
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte(body))
	}))
}

// TestProbeGlobalModelsThreeUARoutesUnion 三路 UA 并发且取并集：
//   - 桌面端主路（默认 UA）+ IDE + CLI 三路各发一次（UA 字面值精确匹配）；
//   - 主路独有模型进并集，IDE/CLI 独有模型也进（这正是本改动目的：三类独有都拿到）；
//   - 重复 id 只出现一次，且**字段取自主路**（IDE/CLI 不得覆盖主路的窗口值）。
func TestProbeGlobalModelsThreeUARoutesUnion(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })

	desktopUA := (&Client{}).defaultWorkBuddyUAFor(globalAcct())
	var calls []string
	srv := uaRoutedSrv(t, map[string]string{
		desktopUA: `{"code":0,"data":{"models":[
			{"id":"shared","name":"S","maxInputTokens":1000000,"maxOutputTokens":128000},
			{"id":"only-desktop","name":"D","maxInputTokens":500000,"maxOutputTokens":64000}
		]}}`,
		ideV3UA: `{"code":0,"data":{"models":[
			{"id":"shared","name":"S","maxInputTokens":176000,"maxOutputTokens":24000},
			{"id":"only-ide","name":"I","maxInputTokens":200000,"maxOutputTokens":32000}
		]}}`,
		cliV3UA: `{"code":0,"data":{"models":[
			{"id":"shared","name":"S","maxInputTokens":272000,"maxOutputTokens":72000},
			{"id":"only-cli","name":"C","maxInputTokens":256000,"maxOutputTokens":32000}
		]}}`,
	}, &calls)
	defer srv.Close()

	c := globalModelsClient(t, srv)
	names := c.FetchGlobalModels(globalAcct())
	infos := c.FetchGlobalModelInfos(globalAcct())

	// 三路 UA 各发一次，字面值精确匹配（缺任一路 = 该路独有模型拿不到）。
	// 只看 /v3/config：企业端点路也用账号默认 UA，按 UA 计数会把它的请求也算进来。
	for _, want := range []string{desktopUA, ideV3UA, cliV3UA} {
		n := 0
		for _, c := range calls {
			if c == methodPathUA(v3ConfigPath, want) {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("UA route %q requested %d times want 1; calls=%v", want, n, calls)
		}
	}

	// 并集：主路原序在前，IDE/CLI 补充项在后，各只出现一次。
	want := []string{"shared", "only-desktop", "only-ide", "only-cli", "enterprise-only"}
	if !sameStrings(names, want) {
		t.Fatalf("union names=%v want %v", names, want)
	}

	// 字段权威：shared 的窗口必须取自主路（IDE 176000 / CLI 272000 不得覆盖）。
	byID := map[string]ModelInfo{}
	for _, mi := range infos {
		byID[mi.ID] = mi
	}
	if got := byID["shared"].ContextWindow; got != 1000000 {
		t.Errorf("shared contextWindow=%d want 1000000（主路字段权威，IDE/CLI 不得覆盖）", got)
	}
	if got := byID["only-ide"].ContextWindow; got != 200000 {
		t.Errorf("only-ide contextWindow=%d want 200000（该 id 只有 IDE 路知道，字段取自该路）", got)
	}
	if got := byID["only-cli"].ContextWindow; got != 256000 {
		t.Errorf("only-cli contextWindow=%d want 256000", got)
	}
}

// TestProbeGlobalModelsPartialUAFailureKeepsOthers 部分路失败不拖垮整次探测：
// IDE 路 400（UA 门禁形态）→ 桌面端与 CLI 的结果照常合并，目录里仍有主路与 CLI 独有模型。
// 这是「并集不劣于单路」的直接断言——没有本例，把 mergeV3Routes 写成「任一路失败即整路
// 放弃」也测不出来。
func TestProbeGlobalModelsPartialUAFailureKeepsOthers(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })

	desktopUA := (&Client{}).defaultWorkBuddyUAFor(globalAcct())
	var calls []string
	// 不给 ideV3UA 配响应 → 该路 400（uaRoutedSrv 的默认分支）。
	srv := uaRoutedSrv(t, map[string]string{
		desktopUA: `{"code":0,"data":{"models":[{"id":"only-desktop","name":"D"}]}}`,
		cliV3UA:   `{"code":0,"data":{"models":[{"id":"only-cli","name":"C"}]}}`,
	}, &calls)
	defer srv.Close()

	names := globalModelsClient(t, srv).FetchGlobalModels(globalAcct())

	want := []string{"only-desktop", "only-cli", "enterprise-only"}
	if !sameStrings(names, want) {
		t.Fatalf("partial-failure union=%v want %v（单路失败不得拖垮其余路）", names, want)
	}
}

// TestGlobalModelsOnceUAOverridesUAOnlyForProbe UA 覆盖只作用于探测请求本身的头，
// 不改 Client 状态：同一 Client 覆盖探测后再发一次默认请求，UA 必须回到账号默认。
// 这条守的是「探测 UA 不得泄漏到 chat 等业务出站路径」——错 UA 可能触发上游 11140。
func TestGlobalModelsOnceUAOverridesUAOnlyForProbe(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })

	desktopUA := (&Client{}).defaultWorkBuddyUAFor(globalAcct())
	var calls []string
	srv := uaRoutedSrv(t, map[string]string{
		desktopUA: `{"code":0,"data":{"models":[{"id":"m","name":"M"}]}}`,
		ideV3UA:   `{"code":0,"data":{"models":[{"id":"m","name":"M"}]}}`,
	}, &calls)
	defer srv.Close()

	c := globalModelsClient(t, srv)
	if _, _, _, _, err := c.globalModelsOnceUA(globalAcct(), v3ConfigPath, ideV3UA); err != nil {
		t.Fatalf("覆盖探测失败: %v", err)
	}
	if c.UserAgent != "" {
		t.Errorf("Client.UserAgent=%q 被探测覆盖改写（应保持空）", c.UserAgent)
	}
	// 默认入口（空 ua）必须用账号默认 UA，而非上一次的覆盖值。
	calls = nil
	if _, _, _, _, err := c.globalModelsOnce(globalAcct(), v3ConfigPath); err != nil {
		t.Fatalf("默认探测失败: %v", err)
	}
	if len(calls) != 1 || !strings.HasSuffix(calls[0], "|"+desktopUA) {
		t.Fatalf("默认探测 UA 未回到账号默认: calls=%v want suffix |%s", calls, desktopUA)
	}
}
