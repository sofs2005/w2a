package upstream

import (
	"net/http"
	"testing"

	"workbuddy2api/internal/auth"
)

// TestNoMachineSessionIDHeaders 出站不再注入 X-Machine-ID / X-Session-ID。
//
// 官方 WorkBuddy Desktop 5.9.3 全量分发包（app.asar）里这两个头名一次都没出现过
// （不分大小写 grep 计数 0）；此前按 uid 稳定派生的自造值反而是最显眼的网关特征。
// 本测试是反向护栏：任何出站路径重新引入这两个头都会红。
func TestNoMachineSessionIDHeaders(t *testing.T) {
	a := &auth.Auth{AccessToken: "at", UID: "u1", RefreshToken: "rt", Domain: "www.workbuddy.ai"}
	c := &Client{}

	for name, build := range map[string]func(r *http.Request){
		"common":  func(r *http.Request) { c.CommonHeaders(r, a) },
		"chat":    func(r *http.Request) { c.ChatHeaders(r, a, "", ChatMeta{}) },
		"billing": func(r *http.Request) { c.BillingHeaders(r, a) },
		"refresh": func(r *http.Request) { c.RefreshHeaders(r, a) },
	} {
		t.Run(name, func(t *testing.T) {
			req := mustRequest(t)
			build(req)
			if got := req.Header.Get("X-Machine-ID"); got != "" {
				t.Errorf("%s X-Machine-ID = %q want empty (官方不发该头)", name, got)
			}
			if got := req.Header.Get("X-Session-ID"); got != "" {
				t.Errorf("%s X-Session-ID = %q want empty (官方不发该头)", name, got)
			}
		})
	}
}
