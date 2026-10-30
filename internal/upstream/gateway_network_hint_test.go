// gateway_network_hint_test.go 钉住三条「本地事实」hint 互不串味（C 段硬要求）。
//
// 背景：上一轮的超时修复之所以必要，正是因为 message 说「rotation stopped (slow
// upstream)」而 hint 说「no healthy account available in pool」——同一份响应把运维
// 指向两个相反的方向。这一轮新增第三条 gateway_network_error，同样必须与另外两条
// 彻底分开：它说的是「网关连不上上游（DNS/拨号/TLS），请求根本没发出去」，换号、
// 换模型都不会好。
package upstream

import (
	"strings"
	"testing"
)

// TestGatewayNetworkHintDistinct 三条本地 hint 两两不同，且各自不含对方的关键词。
func TestGatewayNetworkHintDistinct(t *testing.T) {
	network := GatewayNetworkHint()
	noHealthy := NoHealthyAccountHint()
	timeout := UpstreamTimeoutHint()

	if strings.TrimSpace(network) == "" {
		t.Fatal("gateway_network_error 必须有专用 hint")
	}
	// 两两不同（复用同一句文案 = 排查方向串味）。
	for _, pair := range []struct{ name, a, b string }{
		{"network vs no_healthy", network, noHealthy},
		{"network vs upstream_timeout", network, timeout},
		{"no_healthy vs upstream_timeout", noHealthy, timeout},
	} {
		if pair.a == pair.b {
			t.Errorf("%s: 两条 hint 不得相同: %q", pair.name, pair.a)
		}
	}

	// 不得把「池里没号」的话术带进网关网络错误的 hint（那会让人去翻 /status）。
	for _, bad := range []string{"no healthy account", "check /status"} {
		if strings.Contains(strings.ToLower(network), bad) {
			t.Errorf("gateway 网络错误 hint 不得含 %q: %q", bad, network)
		}
	}
	// 不得把「上游慢/轮转止损」的话术带进来（那会让人等一个不会好转的上游）。
	for _, bad := range []string{"slow upstream", "rotation stopped", "timed out", "before the first byte"} {
		if strings.Contains(strings.ToLower(network), bad) {
			t.Errorf("gateway 网络错误 hint 不得含 %q: %q", bad, network)
		}
	}
	// 反过来：另外两条也不得吞掉「网关出口」这条指向。
	for _, bad := range []string{"dns", "resolv.conf", "could not reach"} {
		if strings.Contains(strings.ToLower(noHealthy), bad) {
			t.Errorf("no_healthy hint 不得含网关出口话术 %q: %q", bad, noHealthy)
		}
		if strings.Contains(strings.ToLower(timeout), bad) {
			t.Errorf("upstream_timeout hint 不得含网关出口话术 %q: %q", bad, timeout)
		}
	}
	// 必须说清「不是账号/不是模型问题」——这是它与另外两条的核心区别。
	lower := strings.ToLower(network)
	if !strings.Contains(lower, "not an account problem") {
		t.Errorf("hint 必须点明与账号无关: %q", network)
	}
	if !strings.Contains(lower, "switching accounts or models will not help") {
		t.Errorf("hint 必须点明换号/换模型无用: %q", network)
	}
	// 必须给出可执行动作（查网关出口 DNS）。
	if !strings.Contains(lower, "resolv.conf") && !strings.Contains(lower, "outbound dns") {
		t.Errorf("hint 必须指向网关出口 DNS: %q", network)
	}
}
