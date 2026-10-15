// oauth.go — WorkBuddy OAuth 设备流的端点与 HTTP 层（按 realm 切换端点/来源头）。
//
// 与 main.go 的分工：本文件只负责「跟上游说话」（常量、请求头、信封解析、取
// 授权 URL、换 token），main.go 负责 CLI 层（参数解析、子命令分发、交互式单命令流程）。
//
// 无 PKCE（workbuddy 设备流由服务端签发 state）。端点 URL 由 realmConfig（main.go）
// 按 realm 拼出后作为 base 传入本文件的函数，故此处不再硬编码端点。
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"strings"
	"time"

	"workbuddy2api/internal/upstream"
)

// 上游常量：CN → copilot.tencent.com（Origin 为 codebuddy.cn）；global → www.workbuddy.ai
// （base 与 Origin/Referer 同域）。base 由 realmConfig 按 realm 选，origin 随之配套。
const (
	upstreamBaseCN      = "https://copilot.tencent.com"
	upstreamBaseGlobal  = "https://www.workbuddy.ai"
	clientUA            = "CLI/2.63.2 CodeBuddy/2.63.2"
	originRefererCN     = "https://www.codebuddy.cn"
	originRefererGlobal = "https://www.workbuddy.ai"
)

// newLoginClient 每个流程独立 cookie jar（多账号登录互不串会话）。
//
// proxyURL 非空时经该代理出站（config.json 的 proxy.<realm> 段，见 proxyForRealm）；
// 空 = 直连（login 是短命 CLI，不接 HTTP_PROXY 环境变量的隐式语义——用户显式配了
// 才走代理，避免「宿主机碰巧设了全局代理」把登录引到意外路径）。
func newLoginClient(proxyURL string) (*http.Client, error) {
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Timeout: 30 * time.Second, Jar: jar}
	if strings.TrimSpace(proxyURL) == "" {
		return c, nil
	}
	tr, err := upstream.ProxyTransportFor(proxyURL)
	if err != nil {
		return nil, err
	}
	c.Transport = tr
	return c, nil
}

// proxyForRealm 读 config.json 的 proxy 段取该 realm 的代理 URL；env 优先
// （WB2A_PROXY_CN / WB2A_PROXY_GLOBAL，与网关 applyEnv 同名同序）。
//
// 读不到 config.json 不是错误：login 常在网关尚未部署时首次运行，此时无代理配置
// 是正常状态（回落直连）。JSON 里 proxy 段的键名与网关 config.go 的 Proxy 结构体
// 对齐（cn / global）。
func proxyForRealm(realm, configPath string) string {
	envKey := "WB2A_PROXY_CN"
	if realm == realmGlobal {
		envKey = "WB2A_PROXY_GLOBAL"
	}
	if v := os.Getenv(envKey); v != "" {
		return v
	}
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return ""
	}
	var doc struct {
		Proxy struct {
			CN     string `json:"cn"`
			Global string `json:"global"`
		} `json:"proxy"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return "" // 配置损坏由网关启动时报错，login 侧静默回落直连
	}
	if realm == realmGlobal {
		return doc.Proxy.Global
	}
	return doc.Proxy.CN
}

// commonHeaders 按 origin 设置通用请求头（Origin/Referer 随 realm 变化）。
// 返回 func(*http.Request)，由调用方按 realm 选定的 origin 构造一次后复用。
func commonHeaders(origin string) func(*http.Request) {
	return func(req *http.Request) {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/plain, */*")
		req.Header.Set("X-Requested-With", "XMLHttpRequest")
		req.Header.Set("Origin", origin)
		req.Header.Set("Referer", origin+"/")
		req.Header.Set("User-Agent", clientUA)
	}
}

// apiEnvelope {code,msg,data} 信封（上游全部业务端点统一形状）。
type apiEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// doJSON 发一次请求并拆 {code,msg,data} 信封：HTTP >=400、重定向、body 读失败、
// 信封解析失败、code!=0 均归为 error（返回的 status 供调用方区分网络层/业务层）。
// body 读失败单独成 "read body" 错误（上游 P0-2 修复）：半截 body 不进 Unmarshal，
// 否则会伪装成 "parse failed" 把网络中断误指为上游格式问题。
func doJSON(client *http.Client, method, fullURL string, headers func(*http.Request), body io.Reader) (json.RawMessage, int, error) {
	req, err := http.NewRequest(method, fullURL, body)
	if err != nil {
		return nil, 0, err
	}
	if headers != nil {
		headers(req)
	} else {
		// 缺省头：CN origin（零回归）
		commonHeaders(originRefererCN)(req)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		// 读失败 → 传输层错误：半截 body 不进 Unmarshal（避免误报 parse failed）。
		return nil, resp.StatusCode, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode >= 400 {
		return nil, resp.StatusCode, fmt.Errorf("http_error: upstream %d", resp.StatusCode)
	}
	if resp.StatusCode >= 300 {
		return nil, resp.StatusCode, fmt.Errorf("http_error: upstream redirect %d", resp.StatusCode)
	}
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("parse failed: %w", err)
	}
	if env.Code != 0 {
		return nil, resp.StatusCode, fmt.Errorf("code=%d msg=%s", env.Code, env.Msg)
	}
	return env.Data, resp.StatusCode, nil
}

// tokenBundle 登录凭证 + 账号信息（换 token 后的聚合结果）。
type tokenBundle struct {
	AccessToken  string
	RefreshToken string
	ExpiresIn    int64
	Domain       string
	UID          string
	EnterpriseID string
	Nickname     string
}

// tokenFields 把 tokenBundle 投影为 buildLoginOutput 的 token 参数
// （buildLoginOutput 用匿名 struct 定形，需逐字对齐字段与 tag）。
func (tb tokenBundle) tokenFields() struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresIn    int64  `json:"expiresIn"`
	Domain       string `json:"domain"`
} {
	return struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
		Domain       string `json:"domain"`
	}{tb.AccessToken, tb.RefreshToken, tb.ExpiresIn, tb.Domain}
}

// accountFields 把 tokenBundle 投影为 buildLoginOutput 的 account 参数。
func (tb tokenBundle) accountFields() struct {
	UID          string `json:"uid"`
	EnterpriseID string `json:"enterpriseId"`
	Nickname     string `json:"nickname"`
} {
	return struct {
		UID          string `json:"uid"`
		EnterpriseID string `json:"enterpriseId"`
		Nickname     string `json:"nickname"`
	}{tb.UID, tb.EnterpriseID, tb.Nickname}
}

// fetchAuthURL 向 base 的 state 端点 POST 取授权 URL 与 state（不落盘）。
// runURL（两段式）与 runOnce（单命令）共用，避免端点组装重复。
func fetchAuthURL(client *http.Client, base, origin string) (authURL, state string, err error) {
	headers := commonHeaders(origin)
	data, _, err := doJSON(client, http.MethodPost, base+"/v2/plugin/auth/state?platform=CLI", headers, bytes.NewReader([]byte("{}")))
	if err != nil {
		return "", "", fmt.Errorf("auth state failed: %w", err)
	}
	var st struct {
		State   string `json:"state"`
		AuthURL string `json:"authUrl"`
	}
	if err := json.Unmarshal(data, &st); err != nil || st.State == "" || st.AuthURL == "" {
		return "", "", fmt.Errorf("auth state: missing state or authUrl")
	}
	return st.AuthURL, st.State, nil
}

// fetchToken 用 state 换 token 与账号信息（不落盘），runPoll 与 runOnce 共用。
// auth/token 是权威登录状态端点，pending 时业务 code 非 0（"login ing"）。
// 判定分层：网络层/5xx → token endpoint error；其余业务失败 → 登录未完成。
func fetchToken(client *http.Client, base, origin, state string) (tokenBundle, error) {
	var out tokenBundle
	headers := commonHeaders(origin)
	tokRaw, status, errTok := doJSON(client, http.MethodGet, base+"/v2/plugin/auth/token?state="+state, headers, nil)
	if errTok != nil {
		if status == 0 || status >= 500 {
			return out, fmt.Errorf("token endpoint error: %w", errTok)
		}
		return out, fmt.Errorf("登录未完成（waiting for login）。请确认已在浏览器完成登录再按 y")
	}
	var tok struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
		Domain       string `json:"domain"`
	}
	if err := json.Unmarshal(tokRaw, &tok); err != nil || tok.AccessToken == "" {
		return out, fmt.Errorf("登录未完成（waiting for login）。请确认已在浏览器完成登录再按 y")
	}
	out.AccessToken = tok.AccessToken
	out.RefreshToken = tok.RefreshToken
	out.ExpiresIn = tok.ExpiresIn
	out.Domain = tok.Domain

	// login/account 拿 uid/nickname（带 Bearer）；失败不阻断——uid 是必需的，下方校验
	var acct struct {
		UID          string `json:"uid"`
		EnterpriseID string `json:"enterpriseId"`
		Nickname     string `json:"nickname"`
	}
	acctHeaders := func(r *http.Request) {
		headers(r)
		r.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	}
	if acctRaw, _, errAcct := doJSON(client, http.MethodGet, base+"/v2/plugin/login/account?state="+state, acctHeaders, nil); errAcct == nil {
		_ = json.Unmarshal(acctRaw, &acct)
	}
	out.UID = acct.UID
	out.EnterpriseID = acct.EnterpriseID
	out.Nickname = acct.Nickname
	if out.UID == "" {
		return out, fmt.Errorf("无法获取 uid，请检查 token 是否有效")
	}
	return out, nil
}
