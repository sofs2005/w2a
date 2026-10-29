package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

// 本文件验证「features.sanitize_extra_*」这条配置链真的接到了出站请求上：
//
//   - 观测手段是**真的发一次 chat 请求**（httptest 假上游），看去达上游的 body 有
//     没有把注入词改写掉。只断言 ConfigureSanitize 被调用过是不够的（那只证明函数
//     活着），要证明它落在 ChatStream 的净化收口上。
//   - 覆盖启动装配（buildUpstream）与配置热重载（ReloadConfig → applyConfig →
//     buildUpstream）两条路径。两者共用 buildUpstream 是设计意图（见其注释），本
//     测试把「共用」变成可执行断言：任一条路径漏接，对应用例就红。

// sanitizeMarker 注入用的假「上游新词」。
const (
	sanitizeMarker = "ZCODE_MARKER"
	markerBodyTmpl = `{"model":"m","stream":true,"messages":[{"role":"user","content":"please mention ` +
		sanitizeMarker + ` here"}]}`
)

// sseStub 最小 SSE 响应（ChatStream 只透传，不在这里解析）。
const sseStub = "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"

// outboundBodyOf 用给定客户端向假上游发一次 chat 请求，返回上游实际收到的 body。
func outboundBodyOf(t *testing.T, c *upstream.Client, body string) string {
	t.Helper()
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = string(b)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(sseStub))
	}))
	defer srv.Close()

	old := c.ChatBaseCN
	c.ChatBaseCN = srv.URL
	defer func() { c.ChatBaseCN = old }()

	acct := &auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"}
	rc, status, raw, err := c.ChatStream(acct, []byte(body), "", upstream.ChatMeta{})
	if rc != nil {
		_ = rc.Close()
	}
	if err != nil || status != http.StatusOK {
		t.Fatalf("假上游请求失败: status=%d err=%v raw=%s", status, err, raw)
	}
	if got == "" {
		t.Fatal("假上游没收到请求体")
	}
	return got
}

// TestBuildUpstreamInjectsSanitizeExtraTerms 启动装配路径：buildUpstream 必须把
// features.sanitize_extra_features / _rewrites 注入成出站净化规则。
func TestBuildUpstreamInjectsSanitizeExtraTerms(t *testing.T) {
	cfg := Default()
	cfg.Features.SanitizeExtraFeatures = []string{sanitizeMarker}
	cfg.Features.SanitizeExtraRewrites = [][2]string{{sanitizeMarker, "WorkBuddy"}}
	up := buildUpstream(cfg)

	got := outboundBodyOf(t, up, markerBodyTmpl)
	if strings.Contains(got, sanitizeMarker) {
		t.Errorf("注入词未被净化（buildUpstream 没接 ConfigureSanitize？）: %s", got)
	}
	if !strings.Contains(got, "WorkBuddy") {
		t.Errorf("注入的替换对未生效: %s", got)
	}

	// 对照组：不带额外词表的客户端把同一句话原样发出去（证明上面的差异确实来自注入，
	// 而不是净化层本来就会改写这段文本）。
	plain := buildUpstream(Default())
	if got := outboundBodyOf(t, plain, markerBodyTmpl); !strings.Contains(got, sanitizeMarker) {
		t.Fatalf("基线客户端不该改写注入词（对照组失效）: %s", got)
	}
}

// TestReloadAppliesSanitizeExtraTerms 热重载路径：面板保存 config.json 后
// ReloadConfig 必须重建上游客户端并重新注入词表（features 段不在重启清单里）。
//
// 这条同时验证「buildUpstream 是启动与热重载共用点」这一设计意图：热重载走
// applyConfig → buildUpstream，注入因而自动生效，不必在两处各写一遍。
func TestReloadAppliesSanitizeExtraTerms(t *testing.T) {
	path, tgt := newReloadFixture(t, `{"api_key":"k"}`)

	// 重载前：基线客户端不改写（确认对照组）。
	if got := outboundBodyOf(t, tgt.up.Load(), markerBodyTmpl); !strings.Contains(got, sanitizeMarker) {
		t.Fatalf("重载前不该改写: %s", got)
	}

	writeConfig(t, path, `{"api_key":"k","features":{`+
		`"sanitize_extra_features":["`+sanitizeMarker+`"],`+
		`"sanitize_extra_rewrites":[["`+sanitizeMarker+`","WorkBuddy"]]}}`)
	a := newConfigReloaderAdapter(path, tgt)
	hot, cold, err := a.ReloadConfig()
	if err != nil {
		t.Fatalf("ReloadConfig: %v", err)
	}
	if len(cold) != 0 {
		t.Fatalf("features 段应可热生效，实际报需重启: %v", cold)
	}
	if len(hot) != 1 || hot[0] != "features" {
		t.Fatalf("hot=%v want [features]", hot)
	}

	if got := outboundBodyOf(t, tgt.up.Load(), markerBodyTmpl); strings.Contains(got, sanitizeMarker) {
		t.Fatalf("热重载后注入词仍未净化（热重载没走 buildUpstream？）: %s", got)
	}
}

// TestReloadClearsSanitizeExtraTerms 清空配置同样热生效：词表整体替换（不是叠加），
// 清空后回到「不命中」，行为与未配置时一致。
func TestReloadClearsSanitizeExtraTerms(t *testing.T) {
	path, tgt := newReloadFixture(t, `{"api_key":"k","features":{`+
		`"sanitize_extra_features":["`+sanitizeMarker+`"],`+
		`"sanitize_extra_rewrites":[["`+sanitizeMarker+`","WorkBuddy"]]}}`)
	if got := outboundBodyOf(t, tgt.up.Load(), markerBodyTmpl); strings.Contains(got, sanitizeMarker) {
		t.Fatalf("初始配置应已生效: %s", got)
	}

	writeConfig(t, path, `{"api_key":"k"}`)
	a := newConfigReloaderAdapter(path, tgt)
	if _, _, err := a.ReloadConfig(); err != nil {
		t.Fatalf("ReloadConfig: %v", err)
	}
	if got := outboundBodyOf(t, tgt.up.Load(), markerBodyTmpl); !strings.Contains(got, sanitizeMarker) {
		t.Fatalf("清空配置后应回到不改写: %s", got)
	}
}
