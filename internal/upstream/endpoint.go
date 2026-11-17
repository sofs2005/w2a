// endpoint.go 云托管企业账号的上游主机路由。
//
// 背景：企业版账号的登录域可能是企业自己的托管子域。官方客户端按 product.json 的
// cloudHostedDomain 通配表识别这类域，并把 refresh / 账号列表 / 模型 / 用量 / 对话
// **全部发到该域**（社区 2026-10 实测；2026-11 在本机 CN 5.11.3 的 product.json 复核：
// cloudHostedDomain = *.sso.copilot.tencent.com / *.copilot.qq.com / *.sso.codebuddy.cn
// 及对应 staging 域）。发到 copilot.tencent.com 的 token 在这些域上无效。
//
// iOA 域（tencent.sso.*，腾讯内部单点）是例外：官方 product.ioa.json 仍指回
// copilot.tencent.com，不能按「子域 = 主机」处理。
package upstream

import (
	"regexp"
	"strings"

	"workbuddy2api/internal/auth"
)

// cloudHostedPatterns 与 product.json cloudHostedDomain 逐条对应（含 staging）。
var cloudHostedPatterns = []*regexp.Regexp{
	regexp.MustCompile(`^[a-z0-9-]+\.sso\.copilot\.tencent\.com$`),
	regexp.MustCompile(`^[a-z0-9-]+\.sso\.copilot-staging\.tencent\.com$`),
	regexp.MustCompile(`^[a-z0-9-]+\.copilot\.qq\.com$`),
	regexp.MustCompile(`^[a-z0-9-]+\.copilot-staging\.qq\.com$`),
	regexp.MustCompile(`^[a-z0-9-]+\.sso\.codebuddy\.cn$`),
	regexp.MustCompile(`^[a-z0-9-]+\.staging-sso\.codebuddy\.cn$`),
}

// ioaHosts 是 iOA 单点域：product.ioa.json 让它们仍走 copilot.tencent.com。
var ioaHosts = map[string]bool{
	"tencent.sso.copilot.tencent.com":         true,
	"tencent.sso.copilot-staging.tencent.com": true,
	"tencent.sso.codebuddy.cn":                true,
	"tencent.staging-sso.codebuddy.cn":        true,
}

// cloudHostedEndpoint 返回账号登录域对应的云托管主机（含 scheme）；
// 非云托管 / iOA / global 域 / 空域 → 空串（调用方回落默认主机）。
func cloudHostedEndpoint(a *auth.Auth) string {
	if a == nil || a.IsGlobal() {
		return ""
	}
	d := strings.ToLower(strings.TrimSpace(a.DomainValue()))
	if d == "" || ioaHosts[d] {
		return ""
	}
	for _, re := range cloudHostedPatterns {
		if re.MatchString(d) {
			return "https://" + d
		}
	}
	return ""
}
