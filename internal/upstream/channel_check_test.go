package upstream

import (
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
)

// 渠道校验的识别与路径兜底（2026-10-17 用户报告）。
//
// 现场：上游对 /v2/chat/completions 新增渠道强校验，被校验到的请求回
// 「没通过渠道校验，可能不是官方客户端发出的」；而官方客户端走的 /console 端点放行。
// 此前这类响应要么落通用 4xx（只换号不罚、坏号留在池里次次失败），要么被封套里的
// `request illegal` 带进「账号被封 → 硬禁用」—— 于是面板上显示「已禁用（需重新登录）」，
// 用户重扫也没用（凭证从来没问题）。
//
// 业务码一律复用 antiprobe_test.go 的构造器：字面量在渲染链路上极易被改写，
// 而它正是判据本身。

// channelCheckBody 一次渠道校验的典型封套：中文「渠道校验」+ 英文 displayTips +
// 历史遗留的 `request illegal` msg（后者正是误禁用账号的触发点）。
func channelCheckBody() string {
	return `{"code":` + bareAntiProbeCode() + `,"msg":"request illegal","requestId":"x",` +
		`"displayMsg":{"zh":"请求被拦截，请重新发送"},` +
		`"displayTips":{"zh":"这次调用没通过渠道校验，可能不是官方客户端发出的。请用官方版本重新发送。",` +
		`"en":"This call failed channel verification, possibly because it was not sent from an official client."}}`
}

func TestIsChannelCheckMatches(t *testing.T) {
	if !IsChannelCheck(channelCheckBody()) {
		t.Fatal("中文「渠道校验」文案必须被识别")
	}
	for _, code := range []string{bareAntiProbeCode(), hyphenAntiProbeCode()} {
		body := `{"code":` + code + `,"displayTips":{"en":"This call failed channel verification"}}`
		if !IsChannelCheck(body) {
			t.Errorf("码形态 %s 没被识别", code)
		}
		quoted := `{"code":"` + code + `","displayTips":{"zh":"没通过渠道校验"}}`
		if !IsChannelCheck(quoted) {
			t.Errorf("引号码形态 %s 没被识别", code)
		}
	}
	// 英文 displayTips 单独也要能命中（中文可能被上游换掉）。
	if !IsChannelCheck(`{"code":` + bareAntiProbeCode() + `,"displayTips":{"en":"possibly because it was not sent from an official client"}}`) {
		t.Error("英文 official client 文案没被识别")
	}
}

func TestIsChannelCheckDoesNotSwallowContentBlock(t *testing.T) {
	// 同一个码族也承载「内容审核拦截」：那种必须保持原行为（不换路径、不罚号、
	// 走降级重试），不能被这条规则吞成账号级。
	for _, content := range []string{
		`{"code":` + bareAntiProbeCode() + `,"msg":"blocked by security policy"}`,
		`{"code":` + hyphenAntiProbeCode() + `,"msg":"request illegal"}`,
	} {
		if IsChannelCheck(content) {
			t.Fatalf("内容审核拦截被误判成渠道校验：%s", content)
		}
		if got := Classify(400, content); got != ErrContentBlocked {
			t.Errorf("内容审核拦截的分类被影响：%v（body=%s）", got, content)
		}
	}
	if IsChannelCheck(`{"msg":"request illegal"}`) {
		t.Error("没有业务码时不该命中")
	}
	if IsChannelCheck(`{"code":` + bareAntiProbeCode() + `,"msg":"请求被拦截"}`) {
		t.Error("没有渠道校验文案时不该命中")
	}
}

func TestClassifyRoutesChannelCheckToAccountFault(t *testing.T) {
	// 归账号级是为了让 handler 的 ErrAccountFault 分支接管（那里最先判它、只软冷却）。
	// 落通用 4xx 的后果是「只换号不罚」：坏号留在池里、每次被选中都失败。
	if got := Classify(400, channelCheckBody()); got != ErrAccountFault {
		t.Errorf("渠道校验应归 ErrAccountFault（供 handler 软冷却），got %v", got)
	}
	// 大小写/引号变体同样归账号级。
	if got := Classify(400, `{"code":"`+bareAntiProbeCode()+`","msg":"conversation id missing"}`); got != ErrAccountFault {
		t.Errorf("conversation id missing 应归 ErrAccountFault，got %v", got)
	}
}

func TestChatPathsConsoleFallbackByRealm(t *testing.T) {
	c := &Client{}
	cn := c.chatPaths(&auth.Auth{UID: "cn1"})
	if len(cn) != 2 || cn[0] != chatCompletionsPath || cn[1] != chatConsolePath {
		t.Fatalf("cn 路径候选应为 [/v2, /console]（/v2 优先，兜底 /console），got %v", cn)
	}
	// global 域 /console 挂腾讯云 WAF 的 body 内容规则（#119）：不启用兜底，
	// 否则渠道校验会被换成 WAF 403 并顺带软冷却一次账号。
	gAcc := &auth.Auth{UID: "g1"}
	gAcc.SetRealm("global")
	c.GlobalEnabled = true
	g := c.chatPaths(gAcc)
	if len(g) != 1 || g[0] != chatCompletionsPath {
		t.Fatalf("global 必须保持单路径 [/v2]，got %v", g)
	}
}

func TestCodeOf(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"数字形态", `{"error":{"data":{"code":11140,"msg":"request illegal"}}}`, "11140"},
		{"字符串形态", `{"code":"11148","msg":"tool calls and tool results do not match"}`, "11148"},
		{"带横线的字符串形态", `{"code":"` + hyphenAntiProbeCode() + `","msg":"conversation id missing"}`, hyphenAntiProbeCode()},
		// 上游偶尔给出**非法 JSON**：code 的值没加引号（issue #201 的原始响应就长这样）。
		// 用 json.Unmarshal 会整条解析失败 → 取不到码，所以这里必须是正则容忍。
		{"裸值（非法 JSON）", `{"code":` + bareAntiProbeCode() + `,"msg":"conversation id missing","requestId":"x"}`, bareAntiProbeCode()},
		{"带空格", `{ "code" : 14018 , "msg" : "no credit" }`, "14018"},
		{"没有 code 字段", `{"msg":"boom"}`, ""},
		{"空体", ``, ""},
		{"HTML 拦截页", `<html><body>403</body></html>`, ""},
		// requestId 里出现数字不该被当成业务码（只认 "code" 键）
		{"只有 requestId", `{"requestId":"11140-abc","msg":"x"}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CodeOf(tc.body); got != tc.want {
				t.Errorf("CodeOf(%q)=%q want %q", tc.body, got, tc.want)
			}
		})
	}
}

// 渠道校验的封套里 simultaneously 出现码与文案时，reason 里要能取到真实码。
func TestUpstreamCodeLabelForChannelCheck(t *testing.T) {
	body := channelCheckBody()
	if code := CodeOf(body); code != bareAntiProbeCode() {
		t.Fatalf("CodeOf=%q want %q", code, bareAntiProbeCode())
	}
	if !strings.Contains(body, "渠道校验") {
		t.Fatal("构造的封套里应含渠道校验文案")
	}
}
