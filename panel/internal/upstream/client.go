// Package upstream 直连 WorkBuddy 上游（CN: copilot.tencent.com / codebuddy.cn，
// GLOBAL: workbuddy.ai）的客户端，用于 GUI 侧的 OAuth 登录、token 刷新、签到、
// 余额查询与猫猫旅行。
//
// 请求头/路径/信封格式与 workbuddy2api/internal/upstream 保持一致，保证同一账号
// 在网关与 GUI 两侧看到的指纹相同、行为可预期。
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"workbuddy2api-gui/internal/authstore"
)

// Region 上游区域。
type Region string

const (
	// RegionCN 国内版（copilot.tencent.com / codebuddy.cn）。
	RegionCN Region = "cn"
	// RegionGlobal 国际版（workbuddy.ai）。
	RegionGlobal Region = "global"
)

// NormalizeRegion 规范化 region 字符串（大小写/空白容错）；非法值返回错误。
func NormalizeRegion(s string) (Region, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "cn":
		return RegionCN, nil
	case "global":
		return RegionGlobal, nil
	default:
		return "", fmt.Errorf("未知区域 %q（可选 cn | global）", s)
	}
}

// 上游常量。
const (
	ChatBaseCN        = "https://copilot.tencent.com"
	BillingBaseCN     = "https://www.codebuddy.cn"
	ChatBaseGlobal    = "https://www.workbuddy.ai"
	BillingBaseGlobal = "https://www.workbuddy.ai"

	clientUA     = "CLI/2.63.2 CodeBuddy/2.63.2"
	originCN     = "https://www.codebuddy.cn"
	originGlobal = "https://www.workbuddy.ai"

	// EndpointAuthState 设备授权：申请 state + authUrl。
	EndpointAuthState = "/v2/plugin/auth/state?platform=CLI"
	// EndpointAuthToken 轮询登录结果（state 由服务端签发，无 PKCE）。
	EndpointAuthToken = "/v2/plugin/auth/token?state="
	// EndpointLoginAccount 拿 uid / nickname / enterpriseId。
	EndpointLoginAccount = "/v2/plugin/login/account?state="

	// EndpointV3Config 模型目录 + 促销配置（/v3/config）。网关用它取模型目录，
	// 但未解析其中的 modelPromotions；GUI 直连取促销（限时免费 / 夜间折扣）。
	EndpointV3Config = "/v3/config"

	// growth 域「猫猫旅行」路径。
	TravelStatusPath   = "/activity/growth/buddy/travel/status"
	TravelDepartPath   = "/activity/growth/buddy/travel/depart"
	TravelClaimPath    = "/activity/growth/buddy/travel/claim"
	BuddyInfoPath      = "/activity/growth/buddy/info"
	BuddyFirstPath     = "/activity/growth/buddy/first"
	BuddyAgreementPath = "/activity/growth/buddy/agreement"

	// TravelLocationID 派出地点固定 4（古镇客栈）：4 个地点收益/时长区间完全相同，无最优解。
	TravelLocationID = 4
)

// buddyTaskIncompleteMarker 领养门槛未达标的业务错误关键词。
const buddyTaskIncompleteMarker = "first_buddy task not completed yet"

// ErrKind 错误分类，用于给前端区分「账号失效」「限流」「余额不足」等处置建议。
type ErrKind int

const (
	// ErrNone 未分类。
	ErrNone ErrKind = iota
	// ErrHardCredit 余额/额度耗尽（HTTP 402 或余额关键词）。
	ErrHardCredit
	// ErrSoftRate 限流（HTTP 429 或限流文案）。
	ErrSoftRate
	// ErrSessionDead 会话失效，需人工重新登录。
	ErrSessionDead
	// ErrNotFound 上游路径 404。
	ErrNotFound
	// ErrServer 上游 5xx。
	ErrServer
	// ErrClient 其余 4xx / 业务 code != 0。
	ErrClient
)

func (k ErrKind) String() string {
	switch k {
	case ErrHardCredit:
		return "余额不足"
	case ErrSoftRate:
		return "被限流"
	case ErrSessionDead:
		return "会话失效（需重新登录）"
	case ErrNotFound:
		return "上游路径不存在"
	case ErrServer:
		return "上游服务端错误"
	case ErrClient:
		return "客户端/业务错误"
	}
	return "未知"
}

// Error 带分类的上游错误。
type Error struct {
	Kind   ErrKind
	Status int
	Msg    string
}

func (e *Error) Error() string {
	if e.Status > 0 {
		return fmt.Sprintf("%s (HTTP %d): %s", e.Kind, e.Status, e.Msg)
	}
	return fmt.Sprintf("%s: %s", e.Kind, e.Msg)
}

// hardMarkers 余额不足关键词（小写匹配）。
var hardMarkers = []string{
	"insufficient", "quota exceeded", "no enough", "not enough credit",
	"balance", "余额不足", "积分不足", "额度不足", "欠费",
}

// softRateMarkers 限流关键词（小写匹配）。
var softRateMarkers = []string{
	"rate limit", "rate-limit", "rate_limit", "too many requests", "too many request",
	"请求过于频繁", "限流", "频率", "slow down", "throttl",
}

// sessionDeadMarkers 会话失效关键词。
var sessionDeadMarkers = []string{
	"offline user session not found", "12153", "session not found", "session expired", "登录已失效",
}

// Classify 按 HTTP 状态码 + body 判定错误类别（判定顺序自严到宽）。
func Classify(status int, body string) ErrKind {
	lower := strings.ToLower(body)
	has := func(markers []string) bool {
		for _, m := range markers {
			if strings.Contains(lower, m) {
				return true
			}
		}
		return false
	}
	// 1. 计费耗尽最严，最先判。
	if status == http.StatusPaymentRequired || has(hardMarkers) {
		return ErrHardCredit
	}
	// 2. 会话失效：需要人工重登的终态，比限流更具体。
	if has(sessionDeadMarkers) {
		return ErrSessionDead
	}
	// 3. 非 429 状态码携带限流文案。
	if has(softRateMarkers) {
		return ErrSoftRate
	}
	switch {
	case status == http.StatusTooManyRequests:
		return ErrSoftRate
	case status == http.StatusUnauthorized:
		// 鉴权层直接拒绝（token 失效/过期）：需要刷新或重新登录，按会话失效归类最可操作。
		return ErrSessionDead
	case status == http.StatusNotFound:
		return ErrNotFound
	case status >= 500:
		return ErrServer
	case status >= 400:
		return ErrClient
	}
	return ErrNone
}

// IsSessionDead 报告错误是否为「会话失效」。
func IsSessionDead(err error) bool {
	var ue *Error
	return errors.As(err, &ue) && ue.Kind == ErrSessionDead
}

// IsAlreadyCheckedIn 判定签到接口返回的是「今日已签到」这类幂等成功。
func IsAlreadyCheckedIn(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	for _, m := range []string{"已签到", "already", "checkin", "code=10001", "签到过"} {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// IsBuddyTaskIncomplete 判定领养门槛未达标（HTTP 400 + first_buddy 关键词）。
func IsBuddyTaskIncomplete(err error) bool {
	var ue *Error
	if !errors.As(err, &ue) || ue.Status != http.StatusBadRequest {
		return false
	}
	return strings.Contains(strings.ToLower(ue.Msg), buddyTaskIncompleteMarker)
}

// Client 上游 HTTP 客户端。
type Client struct {
	HTTP    *http.Client
	Timeout time.Duration
}

// New 构建客户端；timeout <=0 时用 120s。
func New(timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	return &Client{
		HTTP: &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				MaxIdleConns:        50,
				MaxIdleConnsPerHost: 10,
				IdleConnTimeout:     90 * time.Second,
			},
		},
		Timeout: timeout,
	}
}

// apiEnvelope 上游统一信封。
type apiEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// regionBases 返回 region 对应的 chat 基址、billing 基址与 Origin/Referer。
func regionBases(region Region) (chatBase, billingBase, origin string) {
	if region == RegionGlobal {
		return ChatBaseGlobal, BillingBaseGlobal, originGlobal
	}
	return ChatBaseCN, BillingBaseCN, originCN
}

// 官方桌面端 UA 的版本段（与 workbuddy2api 网关的 defaultClientVersion/defaultCliVersion 同值）。
const (
	desktopClientVersion = "5.5.4"
	desktopCLIVersion    = "2.137.1"
)

// promoFetchTimeout 促销配置的单次超时。
//
// 促销只是模型页上的一列装饰，不值得拖住整个模型列表：客户端全局 timeout 默认
// 120 秒，上游一旦卡住，/api/models 会跟着转圈两分钟（两个域还是串行的）。
// 超时后按「拉取失败」处理 —— 模型列表照常返回，只是「优惠」列为空。
const promoFetchTimeout = 15 * time.Second

// desktopUA 组装官方桌面端 UA：`WorkBuddy/<v> <平台品牌>/<v> CLI/<cli>`。
// 平台段品牌按 realm 切——global 用 `WorkBuddy AI`，cn 用 `WorkBuddy`
// （送错品牌段会被上游风控判 403 code 11140）。
func desktopUA(region Region) string {
	platform := "WorkBuddy"
	if region == RegionGlobal {
		platform = "WorkBuddy AI"
	}
	return "WorkBuddy/" + desktopClientVersion + " " + platform + "/" + desktopClientVersion +
		" CLI/" + desktopCLIVersion
}

// desktopHeaders 设置官方桌面端出站请求头（网关 CommonHeaders 的同款口径）。
//
// 为什么不能复用包内既有的 commonHeaders（CLI UA `CLI/2.63.2 CodeBuddy/2.63.2`）：
// 实测同一账号同一时刻，两种 UA 从 /v3/config 拿到的是**两份不同配置**——
//
//	CLI UA      → 21 个模型，无 modelPromotions
//	桌面端 UA   → 23 个模型，3 条 modelPromotions
//
// 模型促销是桌面端面的配置，用 CLI UA 取永远是空的（这正是本页「优惠」列一开始
// 全空的原因）。登录 / 刷新 / 计费仍走既有 commonHeaders，不动它们的指纹。
func desktopHeaders(req *http.Request, region Region) {
	_, _, origin := regionBases(region)
	lang := "zh-CN"
	if region == RegionGlobal {
		lang = "en-US"
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")
	req.Header.Set("User-Agent", desktopUA(region))
	req.Header.Set("Accept-Language", lang)
}

// regionOfAccount 从账号 domain 反推 region；空/未知 domain 按 CN 处理。
func regionOfAccount(a *authstore.Account) Region {
	if a == nil {
		return RegionCN
	}
	d := strings.ToLower(strings.TrimSpace(a.Domain))
	d = strings.TrimPrefix(strings.TrimPrefix(d, "https://"), "http://")
	if d == "workbuddy.ai" || strings.HasSuffix(d, ".workbuddy.ai") {
		return RegionGlobal
	}
	return RegionCN
}

// chatBase 返回账号所属 region 的聊天基址。
func (c *Client) chatBase(a *authstore.Account) string {
	base, _, _ := regionBases(regionOfAccount(a))
	return base
}

// billingBase 返回账号所属 region 的计费基址。
func (c *Client) billingBase(a *authstore.Account) string {
	_, base, _ := regionBases(regionOfAccount(a))
	return base
}

// commonHeaders 设置所有 API 共享的请求头（按 region 设置 Origin/Referer）。
func commonHeaders(req *http.Request, region Region) {
	_, _, origin := regionBases(region)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")
	req.Header.Set("User-Agent", clientUA)
}

// billingHeaders billing / growth 域请求头。
func billingHeaders(req *http.Request, a *authstore.Account) {
	req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	if a.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
		req.Header.Set("X-Tenant-Id", a.EnterpriseID)
	}
	if a.Domain != "" {
		req.Header.Set("X-Domain", a.Domain)
	}
}

// doJSON 发请求并解信封；HTTP 非 2xx 或业务 code != 0 时返回带分类的 *Error。
func (c *Client) doJSON(req *http.Request) (json.RawMessage, error) {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("网络请求失败: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, &Error{
			Kind:   Classify(resp.StatusCode, string(raw)),
			Status: resp.StatusCode,
			Msg:    describeBody(resp.StatusCode, raw),
		}
	}
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("响应解析失败: %w (body: %s)", err, truncate(string(raw), 120))
	}
	if env.Code != 0 {
		kind := Classify(resp.StatusCode, env.Msg)
		if kind == ErrNone {
			kind = ErrClient
		}
		return nil, &Error{
			Kind:   kind,
			Status: resp.StatusCode,
			Msg:    fmt.Sprintf("code=%d msg=%s", env.Code, truncate(env.Msg, 160)),
		}
	}
	return env.Data, nil
}

// describeBody 把上游错误响应体转成可读文案（不含状态码——状态码由 Error.Error() 统一带出）。
//
// 上游在网关层（openresty/apisix）拒绝请求时返回的是 HTML 错误页，而不是 JSON 信封。
// 直接把 HTML 塞进错误信息会让用户看到一大坨标签却看不出问题；这里识别 HTML 后
// 换成人话，并保留可操作的处置建议（401 通常意味着 token 失效）。
func describeBody(status int, raw []byte) string {
	body := strings.TrimSpace(string(raw))
	looksHTML := strings.HasPrefix(body, "<") || strings.Contains(strings.ToLower(body), "<html")
	if looksHTML {
		switch status {
		case http.StatusUnauthorized, http.StatusForbidden:
			return "上游拒绝授权（token 可能已失效，请刷新或重新登录）"
		case http.StatusNotFound:
			return "上游接口不存在（域名或路径可能已变更）"
		default:
			return "上游返回了非 JSON 的错误页"
		}
	}
	if body == "" {
		return "上游返回空响应"
	}
	return truncate(body, 200)
}

// ---------------------------------------------------------------------------
// OAuth 设备授权登录
// ---------------------------------------------------------------------------

// StartLogin 申请设备授权，返回 state 与用户需在浏览器打开的授权 URL。
func (c *Client) StartLogin(region Region) (state, authURL string, err error) {
	base, _, _ := regionBases(region)
	req, err := http.NewRequest(http.MethodPost, base+EndpointAuthState, bytes.NewReader([]byte("{}")))
	if err != nil {
		return "", "", err
	}
	commonHeaders(req, region)
	data, err := c.doJSON(req)
	if err != nil {
		return "", "", err
	}
	var st struct {
		State   string `json:"state"`
		AuthURL string `json:"authUrl"`
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return "", "", fmt.Errorf("授权响应解析失败: %w", err)
	}
	if st.State == "" {
		return "", "", fmt.Errorf("授权响应缺少 state")
	}
	// 上游偶发缺 authUrl（尤其 global），此时用 base 兜底拼一个登录页。
	url := st.AuthURL
	if url == "" {
		url = base + "/login?state=" + st.State + "&platform=CLI"
	}
	return st.State, url, nil
}

// PollLogin 轮询一次登录结果。
//
// 返回 (nil, nil) 表示用户尚未完成登录（预期状态，前端继续轮询）；
// 返回非 nil Account 表示登录成功（accessToken/uid 已填充，但尚未落盘）。
func (c *Client) PollLogin(region Region, state string) (*authstore.Account, error) {
	if strings.TrimSpace(state) == "" {
		return nil, fmt.Errorf("缺少 state")
	}
	base, _, _ := regionBases(region)

	req, err := http.NewRequest(http.MethodGet, base+EndpointAuthToken+state, nil)
	if err != nil {
		return nil, err
	}
	commonHeaders(req, region)
	data, err := c.doJSON(req)
	if err != nil {
		var ue *Error
		// 服务端「登录未完成」以业务 code != 0 表达；5xx/网络错误才是真失败。
		if errors.As(err, &ue) && ue.Status > 0 && ue.Status < 500 {
			return nil, nil
		}
		return nil, err
	}
	var tok struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
		Domain       string `json:"domain"`
	}
	if err := json.Unmarshal(data, &tok); err != nil || tok.AccessToken == "" {
		// 拿不到 token 视为尚未完成，让前端继续轮询。
		return nil, nil
	}
	// global 上游可能不返回 domain，按 region 兜底写入，保证 regionOfAccount 判得准。
	if tok.Domain == "" {
		if region == RegionGlobal {
			tok.Domain = "www.workbuddy.ai"
		} else {
			tok.Domain = "copilot.tencent.com"
		}
	}
	acct := &authstore.Account{
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		Domain:       tok.Domain,
	}
	if tok.ExpiresIn > 0 {
		acct.ExpiresAt = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix()
	}
	// login/account 拿 uid / nickname / enterpriseId（带 Bearer；失败不视为登录失败）。
	acctReq, err := http.NewRequest(http.MethodGet, base+EndpointLoginAccount+state, nil)
	if err == nil {
		commonHeaders(acctReq, region)
		acctReq.Header.Set("Authorization", "Bearer "+tok.AccessToken)
		if resp, err := c.HTTP.Do(acctReq); err == nil {
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			var env apiEnvelope
			if json.Unmarshal(raw, &env) == nil && env.Code == 0 {
				var info struct {
					UID          string `json:"uid"`
					EnterpriseID string `json:"enterpriseId"`
					Nickname     string `json:"nickname"`
				}
				if json.Unmarshal(env.Data, &info) == nil {
					acct.UID = info.UID
					acct.EnterpriseID = info.EnterpriseID
					acct.Nickname = info.Nickname
				}
			}
		}
	}
	if acct.UID == "" {
		return nil, fmt.Errorf("登录成功但未能获取 uid，请稍后重试或改用 login.sh")
	}
	return acct, nil
}

// ---------------------------------------------------------------------------
// Token / 签到 / 余额
// ---------------------------------------------------------------------------

// RefreshToken 刷新 access token，就地更新 a（不落盘，调用方负责 Save）。
func (c *Client) RefreshToken(a *authstore.Account) error {
	if strings.TrimSpace(a.RefreshToken) == "" {
		return fmt.Errorf("账号缺少 refreshToken，无法刷新（需重新登录）")
	}
	region := regionOfAccount(a)
	req, err := http.NewRequest(http.MethodPost, c.chatBase(a)+"/v2/plugin/auth/token/refresh", nil)
	if err != nil {
		return err
	}
	commonHeaders(req, region)
	req.Header.Set("X-Refresh-Token", a.RefreshToken)
	if a.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
	}
	req.Header.Set("X-Auth-Refresh-Source", "workbuddy")

	data, err := c.doJSON(req)
	if err != nil {
		return err
	}
	var tok struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
		Domain       string `json:"domain"`
	}
	if err := json.Unmarshal(data, &tok); err != nil || tok.AccessToken == "" {
		return fmt.Errorf("刷新失败：响应无 accessToken，需重新登录")
	}
	a.AccessToken = tok.AccessToken
	if tok.RefreshToken != "" {
		a.RefreshToken = tok.RefreshToken
	}
	if tok.Domain != "" {
		a.Domain = tok.Domain
	}
	// 响应缺 expiresIn 时保留旧过期时间，避免刷新风暴。
	if tok.ExpiresIn > 0 {
		a.ExpiresAt = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix()
	}
	return nil
}

// CheckinResult 签到结果。
type CheckinResult struct {
	Already bool
	Message string
	Raw     string
}

// DailyCheckin 执行每日签到。已签到不视为失败，返回 Already=true。
func (c *Client) DailyCheckin(a *authstore.Account) (*CheckinResult, error) {
	req, err := http.NewRequest(http.MethodPost, c.billingBase(a)+"/v2/billing/meter/daily-checkin", bytes.NewReader([]byte("{}")))
	if err != nil {
		return nil, err
	}
	billingHeaders(req, a)
	data, err := c.doJSON(req)
	if err != nil {
		if IsAlreadyCheckedIn(err) {
			return &CheckinResult{Already: true, Message: err.Error()}, nil
		}
		return nil, err
	}
	raw := strings.TrimSpace(string(data))
	msg := "签到成功"
	if raw != "" && raw != "null" {
		msg = "签到成功：" + truncate(raw, 160)
	}
	return &CheckinResult{Message: msg, Raw: raw}, nil
}

// resourcePackage 单个套餐的容量字段。
type resourcePackage struct {
	PackageName string `json:"PackageName"`
	// CycleEndTime 积分包**周期**结束时间（"2006-01-02 15:04:05"）。
	//
	// ⚠️ 它不总是「到期时间」：按周期发量的包（实测「CodeBuddy个人体验版」）这里是本月
	// 周期边界，而真正的扣费截止在 DeductionEndTime。判定到期一律用 expiryString()。
	CycleEndTime string `json:"CycleEndTime"`
	// DeductionEndTime 扣费截止（毫秒时间戳）。**这是真正的到期时刻**。
	//
	// 实测（2026-09-23，抽查 12 个积分包）：11 个 CycleEndTime == DeductionEndTime，
	// 唯一例外是「个人体验版」——CycleEndTime 2026-09-30（月周期边界）、
	// DeductionEndTime 2034-12-22。按 DeductionEndTime 排序的结果与官方「平台奖励积分明细」
	// 面板逐行一致；按 CycleEndTime 排则会把这个包错排到最前面，虚报「7 天后作废 500」。
	DeductionEndTime    int64 `json:"DeductionEndTime"`
	CapacityRemain      int64 `json:"CapacityRemain"`
	CapacityUsed        int64 `json:"CapacityUsed"`
	CapacitySize        int64 `json:"CapacitySize"`
	CycleCapacityRemain int64 `json:"CycleCapacityRemain"`
	CycleCapacityUsed   int64 `json:"CycleCapacityUsed"`
	CycleCapacitySize   int64 `json:"CycleCapacitySize"`
}

// Credits 账号积分概览。
type Credits struct {
	Remain   int64 `json:"remain"`
	Used     int64 `json:"used"`
	Size     int64 `json:"size"`
	Packages int   `json:"packages"`

	// ExpiresAt 最近一次积分到期时间，原样保留上游字符串（空 = 所有包都无到期）。
	ExpiresAt string `json:"expires_at,omitempty"`
	// ExpiringRemain 与 ExpiresAt 同一时刻到期的那批积分剩余量。
	ExpiringRemain int64 `json:"expiring_remain"`
	// Details 逐包明细，按到期时间升序，无到期的排最后。
	Details []CreditPack `json:"details,omitempty"`
}

// expiryString 返回该包真正的到期时间（creditPackLayout 格式）。
//
// 优先 DeductionEndTime（扣费截止，毫秒时间戳）；缺省/为 0 时回退 CycleEndTime 原文。
// 两者都没有 → 返回空，调用方按「无到期」处理（不编造）。
func (p resourcePackage) expiryString() string {
	if p.DeductionEndTime > 0 {
		return time.UnixMilli(p.DeductionEndTime).Format(creditPackLayout)
	}
	return p.CycleEndTime
}

// CreditPack 单个积分包的到期明细。
type CreditPack struct {
	Name string `json:"name,omitempty"`
	// EndTime 到期时间（creditPackLayout 原文），= DeductionEndTime 优先。
	EndTime string `json:"end_time,omitempty"`
	// CycleEndTime 上游下发的周期结束时间原文。与 EndTime 不同时才需要展示
	// （说明这个包是按周期发量的，EndTime 才是真到期）。
	CycleEndTime string `json:"cycle_end_time,omitempty"`
	Remain       int64  `json:"remain"`
	Size         int64  `json:"size"`
}

// creditPackLayout 上游 CycleEndTime 的格式（与 billing 请求体里的时间格式同源）。
const creditPackLayout = "2006-01-02 15:04:05"

// summarizeExpiry 从逐包明细里挑出最近一次**有余额的**到期：解析成功且晚于 now 的最小时间，
// 并把同一时刻到期的包剩余量合并计入 expiring。没有这样的包 → 返回空。
//
// 为什么跳过 remain <= 0 的包：一个已经用光的包到期时丢不了任何东西，但它的到期时间
// 往往最早（实测：最早到期的是 9/30 的 0 余额包，而真正有余额的是 10/1 的 45 分）。
// 拿它当「最近到期」会把有余额的那批挤出视野，正好把这一栏的用途反掉了。
func summarizeExpiry(packs []CreditPack, now time.Time) (at string, expiring int64) {
	best := time.Time{}
	for _, p := range packs {
		if p.EndTime == "" || p.Remain <= 0 {
			continue
		}
		t, err := time.ParseInLocation(creditPackLayout, p.EndTime, now.Location())
		if err != nil || !t.After(now) {
			continue
		}
		if best.IsZero() || t.Before(best) {
			best, at, expiring = t, p.EndTime, p.Remain
			continue
		}
		if t.Equal(best) {
			expiring += p.Remain
		}
	}
	return at, expiring
}

// sortPacksByExpiry 按到期时间升序重排明细；无到期（或解析失败）的排最后，组内保持原序。
func sortPacksByExpiry(packs []CreditPack) []CreditPack {
	key := func(p CreditPack) (time.Time, bool) {
		if p.EndTime == "" {
			return time.Time{}, false
		}
		t, err := time.ParseInLocation(creditPackLayout, p.EndTime, time.Local)
		if err != nil {
			return time.Time{}, false
		}
		return t, true
	}
	sort.SliceStable(packs, func(i, j int) bool {
		ti, oki := key(packs[i])
		tj, okj := key(packs[j])
		if oki != okj {
			return oki // 有到期的在前
		}
		if !oki {
			return false
		}
		return ti.Before(tj)
	})
	return packs
}

// UserResource 查询账号可花费积分余额（所有套餐聚合，负值钳 0）。
func (c *Client) UserResource(a *authstore.Account) (*Credits, error) {
	now := time.Now()
	body, _ := json.Marshal(map[string]any{
		"PageNumber":               1,
		"PageSize":                 100,
		"ProductCode":              "p_tcaca",
		"Status":                   []int{0, 3},
		"PackageEndTimeRangeBegin": now.Format("2006-01-02 15:04:05"),
		"PackageEndTimeRangeEnd":   now.Add(365 * 101 * 24 * time.Hour).Format("2006-01-02 15:04:05"),
	})
	req, err := http.NewRequest(http.MethodPost, c.billingBase(a)+"/v2/billing/meter/get-user-resource", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	billingHeaders(req, a)
	data, err := c.doJSON(req)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Response struct {
			Data struct {
				TotalDosage int64             `json:"TotalDosage"`
				Accounts    []resourcePackage `json:"Accounts"`
			} `json:"Data"`
		} `json:"Response"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("积分响应解析失败: %w", err)
	}
	out := &Credits{Packages: len(resp.Response.Data.Accounts)}
	for _, p := range resp.Response.Data.Accounts {
		remain, used, size := packageRemainUsed(p)
		out.Remain += remain
		out.Used += used
		out.Size += size
		pack := CreditPack{
			Name:         p.PackageName,
			EndTime:      p.expiryString(),
			CycleEndTime: p.CycleEndTime,
			Remain:       remain,
			Size:         size,
		}
		out.Details = append(out.Details, pack)
	}
	out.Details = sortPacksByExpiry(out.Details)
	out.ExpiresAt, out.ExpiringRemain = summarizeExpiry(out.Details, now)
	if out.Size > 0 {
		if derived := out.Size - out.Remain; derived > out.Used {
			out.Used = derived
		}
	}
	if dosage := resp.Response.Data.TotalDosage; dosage > out.Size {
		out.Size = dosage
		if derived := out.Size - out.Remain; derived > out.Used {
			out.Used = derived
		}
	}
	if out.Remain < 0 {
		out.Remain = 0
	}
	return out, nil
}

// PromoWindow 促销的每日时段窗口（"23:00" ~ "7:50"，跨零点由 start > end 表达）。
type PromoWindow struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

// ModelPromotion 上游 /v3/config 下发的模型促销条目。
//
// 两种形态（由 Schedule 的字段区分）：
//   - 日期区间型：ValidFrom/ValidUntil 有值（如「限时免费至 9-25」）
//   - 每日时段型：Daily 有值（如「每晚 23:00—次日 8:00 五折」）
//
// Factor 是折扣系数：0 = 免费，0.5 = 五折，1 = 无折扣。
type ModelPromotion struct {
	ID         string   `json:"id"`
	ModelIDs   []string `json:"model_ids"`
	Enabled    bool     `json:"enabled"`
	Kind       string   `json:"kind,omitempty"`
	Priority   int      `json:"priority,omitempty"`
	BadgeLabel string   `json:"badge_label,omitempty"`
	BadgeColor string   `json:"badge_color,omitempty"`
	// HasDiscount 上游是否下发了 discount 块。
	//
	// 有若干条目只带 badge、不带 discount（上游用它做「时段内/时段外」两张脸，
	// 例如 glm-5.2 的夜间折扣在白天挂一张同名的 daytime-badge）。
	// 这些条目 factor 会缺省为 0，若不区分就会把「白天原价」误报成「免费」。
	HasDiscount bool `json:"has_discount,omitempty"`
	// Factor 折扣系数：0 = 免费，0.5 = 五折，1 = 无折扣。
	//
	// 指针 + omitempty：没有 discount 块的条目直接不下发这个字段。若用 float64 的
	// 零值，JSON 里会出现「has_discount:false 但 factor:0」—— 单看 factor 会读成
	// 「免费」，正是上面说的那个误报。字段缺席比给个会被误读的 0 诚实。
	Factor            *float64      `json:"factor,omitempty"`
	DiscountedCredits string        `json:"discounted_credits,omitempty"`
	ValidFrom         string        `json:"valid_from,omitempty"`
	ValidUntil        string        `json:"valid_until,omitempty"`
	Daily             []PromoWindow `json:"daily,omitempty"`
	Timezone          string        `json:"timezone,omitempty"`
	Text              string        `json:"text,omitempty"` // hover.textZh 原文
}

// Promotions 拉取账号所在域的模型促销配置。
//
// 数据源是 /v3/config 的 data.modelPromotions —— 与网关 /v1/models 的模型目录同源，
// 但网关只解析 models/agents，未解析这一段，所以 GUI 自己直连取（与积分查询同思路）。
// 促销是营销配置，调用方负责缓存；本方法不做缓存。
func (c *Client) Promotions(a *authstore.Account) ([]ModelPromotion, error) {
	region := regionOfAccount(a)
	req, err := http.NewRequest(http.MethodGet, c.chatBase(a)+EndpointV3Config, nil)
	if err != nil {
		return nil, err
	}
	// 必须用桌面端头：CLI UA 拿到的配置里没有 modelPromotions（见 desktopHeaders 注释）。
	desktopHeaders(req, region)
	req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	req.Header.Set("X-CodeBuddy-Request", "1")

	ctx, cancel := context.WithTimeout(context.Background(), promoFetchTimeout)
	defer cancel()
	req = req.WithContext(ctx)

	data, err := c.doJSON(req)
	if err != nil {
		return nil, err
	}
	// 注意 doJSON 已经把信封剥掉了（返回 data 本身），所以这里直接解顶层 modelPromotions。
	var resp struct {
		Promotions []struct {
			ID       string   `json:"id"`
			ModelIDs []string `json:"modelIds"`
			Enabled  bool     `json:"enabled"`
			Kind     string   `json:"kind"`
			Priority int      `json:"priority"`
			Badge    struct {
				Label string `json:"label"`
				Color string `json:"color"`
			} `json:"badge"`
			// 指针：区分「上游没给 discount 块」与「给了但 factor=0（真免费）」。
			Discount *struct {
				Factor            float64 `json:"factor"`
				DiscountedCredits string  `json:"discountedCredits"`
			} `json:"discount"`
			Schedule struct {
				Timezone   string `json:"timezone"`
				ValidFrom  string `json:"validFrom"`
				ValidUntil string `json:"validUntil"`
				Daily      []struct {
					Start string `json:"start"`
					End   string `json:"end"`
				} `json:"daily"`
			} `json:"schedule"`
			Hover struct {
				TextZh string `json:"textZh"`
			} `json:"hover"`
		} `json:"modelPromotions"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("促销配置解析失败: %w", err)
	}
	out := make([]ModelPromotion, 0, len(resp.Promotions))
	for _, p := range resp.Promotions {
		if !p.Enabled {
			continue // 停用的促销不下发，避免页面展示已下线的活动
		}
		mp := ModelPromotion{
			ID:         p.ID,
			ModelIDs:   p.ModelIDs,
			Enabled:    p.Enabled,
			Kind:       p.Kind,
			Priority:   p.Priority,
			BadgeLabel: p.Badge.Label,
			BadgeColor: p.Badge.Color,
			ValidFrom:  p.Schedule.ValidFrom,
			ValidUntil: p.Schedule.ValidUntil,
			Timezone:   p.Schedule.Timezone,
			Text:       p.Hover.TextZh,
		}
		// 已过期的促销不下发：上游会把结束的活动继续挂在配置里（实测 hy4-preview
		// 的限免 2026-09-08 已结束但仍在列表），照搬会让页面显示一个假的「限时免费」。
		if end, ok := parseRFC3339(mp.ValidUntil); ok && end.Before(time.Now()) {
			continue
		}
		if p.Discount != nil {
			mp.HasDiscount = true
			mp.Factor = &p.Discount.Factor
			mp.DiscountedCredits = p.Discount.DiscountedCredits
		}
		for _, w := range p.Schedule.Daily {
			mp.Daily = append(mp.Daily, PromoWindow{Start: w.Start, End: w.End})
		}
		out = append(out, mp)
	}
	return out, nil
}

// parseRFC3339 解析促销里的 schedule 时间（带时区偏移，如 "2026-09-25T00:00:00+08:00"）。
// 解析失败返回 ok=false —— 上游格式变了也不该让整条促销消失。
func parseRFC3339(s string) (time.Time, bool) {
	if strings.TrimSpace(s) == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// packageRemainUsed 按 Cycle* 优先的口径拆出 remain/used/size
// （与 workbuddy2api/cmd/credit 及网关 UserResource 的聚合口径保持一致）。
func packageRemainUsed(p resourcePackage) (remain, used, size int64) {
	if p.CycleCapacitySize > 0 {
		remain = p.CycleCapacityRemain
		size = p.CycleCapacitySize
		if remain < 0 {
			remain = 0
		}
		if remain > size {
			remain = size
		}
		used = size - remain
		if p.CycleCapacityUsed > used {
			used = p.CycleCapacityUsed
			if size >= used {
				remain = size - used
			}
		}
		return remain, used, size
	}
	remain = p.CapacityRemain
	used = p.CapacityUsed
	size = p.CapacitySize
	if used == 0 && size > remain {
		used = size - remain
	}
	return remain, used, size
}

// ---------------------------------------------------------------------------
// 猫猫旅行
// ---------------------------------------------------------------------------

// Buddy 账号当前猫档案；nil 表示无猫。
type Buddy struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// TravelState 猫猫旅行状态。
type TravelState struct {
	State             string `json:"state"`               // idle / traveling / arrived
	DailyLimitReached bool   `json:"daily_limit_reached"` // 今日已派出过（CST 自然日重置）
	RecordID          int64  `json:"record_id"`
	RewardCredit      int64  `json:"reward_credit"`
}

// growthJSON 发 growth 域请求并解信封。
func (c *Client) growthJSON(a *authstore.Account, method, path string, body any) (json.RawMessage, error) {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, c.chatBase(a)+path, rdr)
	if err != nil {
		return nil, err
	}
	billingHeaders(req, a)
	return c.doJSON(req)
}

// TravelStatus 查询猫猫旅行状态。
func (c *Client) TravelStatus(a *authstore.Account) (*TravelState, error) {
	data, err := c.growthJSON(a, http.MethodGet, TravelStatusPath, nil)
	if err != nil {
		return nil, err
	}
	var st TravelState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("旅行状态解析失败: %w", err)
	}
	return &st, nil
}

// TravelDepart 派出猫旅行。
func (c *Client) TravelDepart(a *authstore.Account, locationID int) error {
	_, err := c.growthJSON(a, http.MethodPost, TravelDepartPath, map[string]any{"location_id": locationID})
	return err
}

// TravelClaim 领取到站奖励，返回 reward_credit。
func (c *Client) TravelClaim(a *authstore.Account, recordID int64) (int64, error) {
	data, err := c.growthJSON(a, http.MethodPost, TravelClaimPath, map[string]any{"record_id": recordID})
	if err != nil {
		return 0, err
	}
	var resp struct {
		RewardCredit int64 `json:"reward_credit"`
	}
	if len(data) > 0 {
		_ = json.Unmarshal(data, &resp)
	}
	return resp.RewardCredit, nil
}

// BuddyInfo 查询当前猫档案；返回 (nil, nil) 表示无猫。
func (c *Client) BuddyInfo(a *authstore.Account) (*Buddy, error) {
	data, err := c.growthJSON(a, http.MethodGet, BuddyInfoPath, nil)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Buddy json.RawMessage `json:"buddy"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, err
	}
	trimmed := strings.TrimSpace(string(resp.Buddy))
	if trimmed == "" || trimmed == "null" {
		return nil, nil
	}
	var b Buddy
	if err := json.Unmarshal(resp.Buddy, &b); err != nil {
		return nil, err
	}
	return &b, nil
}

// BuddyFirst 领养第一只猫（门槛未达标返回 HTTP 400，属预期）。
func (c *Client) BuddyFirst(a *authstore.Account) error {
	_, err := c.growthJSON(a, http.MethodPost, BuddyFirstPath, map[string]any{})
	return err
}

// BuddyAgreement 同意协议（幂等）。
func (c *Client) BuddyAgreement(a *authstore.Account) error {
	_, err := c.growthJSON(a, http.MethodPost, BuddyAgreementPath, map[string]any{"agree": true})
	return err
}

// TravelResult 单账号一次旅行巡检结果。
type TravelResult struct {
	UID     string `json:"uid"`
	Action  string `json:"action"` // adopt / depart / claim / skip / error
	Message string `json:"message"`
	Reward  int64  `json:"reward,omitempty"`
	Buddy   string `json:"buddy,omitempty"`
}

// TravelOnce 对单个账号推进一趟旅行状态机（单趟只做一个动作，不轮询不等待）。
//
// 状态机与 workbuddy2api/internal/scheduler/travel.go 对齐：
//
//	无猫            → 同意协议 + 尝试领养（门槛未达标属预期，静默跳过）
//	state=idle      → 今日未派出则派出（location_id=4）
//	state=arrived   → 领取到站奖励
//	state=traveling / 今日已达上限 → 跳过
func (c *Client) TravelOnce(a *authstore.Account) (*TravelResult, error) {
	res := &TravelResult{UID: a.UID}
	buddy, err := c.BuddyInfo(a)
	if err != nil {
		res.Action = "error"
		res.Message = "查询猫档案失败: " + err.Error()
		return res, err
	}
	if buddy == nil {
		// 先同意协议（幂等），再尝试领养。
		if err := c.BuddyAgreement(a); err != nil {
			res.Action = "error"
			res.Message = "同意协议失败: " + err.Error()
			return res, err
		}
		if err := c.BuddyFirst(a); err != nil {
			if IsBuddyTaskIncomplete(err) {
				res.Action = "skip"
				res.Message = "领养门槛未达标（今日不再重试）"
				return res, nil
			}
			res.Action = "error"
			res.Message = "领养失败: " + err.Error()
			return res, err
		}
		res.Action = "adopt"
		res.Message = "领养成功（+300 积分）"
		return res, nil
	}
	res.Buddy = buddy.Name

	st, err := c.TravelStatus(a)
	if err != nil {
		res.Action = "error"
		res.Message = "查询旅行状态失败: " + err.Error()
		return res, err
	}
	switch st.State {
	case "idle":
		if st.DailyLimitReached {
			res.Action = "skip"
			res.Message = "今日已派出，等待归来"
			return res, nil
		}
		if err := c.TravelDepart(a, TravelLocationID); err != nil {
			if IsBuddyTaskIncomplete(err) {
				res.Action = "skip"
				res.Message = "派出条件未满足，今日跳过"
				return res, nil
			}
			res.Action = "error"
			res.Message = "派出失败: " + err.Error()
			return res, err
		}
		res.Action = "depart"
		res.Message = "已派猫出门（古镇客栈）"
		return res, nil
	case "arrived":
		reward, err := c.TravelClaim(a, st.RecordID)
		if err != nil {
			res.Action = "error"
			res.Message = "领奖失败: " + err.Error()
			return res, err
		}
		res.Action = "claim"
		res.Reward = reward
		res.Message = fmt.Sprintf("已领取到站奖励 +%d", reward)
		return res, nil
	case "traveling":
		res.Action = "skip"
		res.Message = "猫正在旅行中"
		return res, nil
	default:
		res.Action = "skip"
		res.Message = fmt.Sprintf("未知状态 %q，跳过", st.State)
		return res, nil
	}
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) > n {
		return s[:n]
	}
	return s
}
