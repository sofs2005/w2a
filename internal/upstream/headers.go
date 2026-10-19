// Package headers 构造三类上游请求头（common / chat / billing / refresh）。
// 规则来自 docs/api-reference.md §0/§4/§6。
package upstream

import (
	"net/http"
	"strings"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/session"
)

const (
	// defaultClientVersionCN 国内版出站 WorkBuddy 客户端版本段（UA 的 `WorkBuddy/<ver>`
	// 与白名单头组的 X-IDE-Version）。对齐官方国内版分发包实测版本
	// （~/.workbuddy/last-launch.json version = 5.8.3，2026-10-16）。两域版本不同步，
	// 故按 realm 分开内置。config upstream.client_version 可覆盖（空 = 按 realm 取内置）。
	defaultClientVersionCN = "5.8.3"
	// defaultClientVersionGlobal 国际版出站 WorkBuddy 客户端版本段。对齐官方国际版
	// 分发包实测版本（~/.workbuddy-ai/last-launch.json version = 5.8.4，2026-10-17）。
	defaultClientVersionGlobal = "5.8.4"
	// defaultCliVersion 出站 UA 中 `CLI/<ver>` 段版本。两域共用的官方内置 CLI 版本
	// 实测均为 2.161.0（cli/package.json publishConfig.customPackage.version）。
	// config upstream.cli_version 可覆盖（空 = 内置默认）。
	defaultCliVersion = "2.161.0"

	originRefererCN     = "https://www.codebuddy.cn"
	originRefererGlobal = "https://www.workbuddy.ai"

	// uaAppNameCN / uaAppNameGlobal 出站 UA 第二段的「应用名」。
	// 来源：官方桌面端 main/sidecar-manager.js 的 getCachedBaseEnv() 把 CLI 的
	// CLIENT_INFO_* 环境变量注入子进程，其中 CLIENT_INFO_USER_AGENT_EXTENSION=`CLI/<ver>`；
	// CLI 侧 UserAgentHttpInterceptor.buildUserAgent() 再按
	//   `${platform}/${platformVersion} ${applicationName}/${productVersion} ${uaExtension}`
	// 拼出最终 UA（dist/codebuddy-lite-wb.mjs: buildUserAgent）。
	// 关键：applicationName 直接取自各自 product.json，**两域不同值**——
	//   CN     product.json: "applicationName": "WorkBuddy"     → WorkBuddy/5.8.3
	//   global product.json: "applicationName": "workbuddy-ai"  → workbuddy-ai/5.8.4
	// 而 platform 段两域都硬编码 "WorkBuddy"（sidecar-manager.js: var mt=`WorkBuddy`），
	// 所以只有第二段随 realm 变。此前按「两域 server.js 逐字节相同」推断第二段恒为
	// WorkBuddy 是搞错了链路：server.js 的 buildVlmGatewayHeaders 只服务本地 VLM 技能
	// 代理，桌面端主对话走 sidecar→CLI 直连，UA 由后者决定。
	uaAppNameCN     = "WorkBuddy"
	uaAppNameGlobal = "workbuddy-ai"

	// OpenAI Node SDK 的 X-Stainless-* 指纹常量。来源：官方国际版分发包
	// resources/app.asar.unpacked/cli/dist/lazy-lite-wb/3043.50ee4dca.mjs 的
	// OpenAI 客户端 buildHeaders()——它对每个模型请求无条件拼入该族：
	//   X-Stainless-Lang / Package-Version / OS / Arch / Runtime / Runtime-Version
	// 其中 OS/Arch 由 normalizePlatform(process.platform) / normalizeArch(process.arch)
	// 归一（win32→Windows、x64→x64），Runtime 恒 "node"，Package-Version 恒 "6.25.0"。
	//
	// 关键：只有 CODEBUDDY_SKIP_INTERNAL_HEADERS（ma.hv）为真时，axiosToFetchAdapter
	// 才在 er 分支按 `x-stainless-` 前缀把整族剥掉（见其 transformRequest 注入）。
	// 该 env 默认不设 → 官方默认安装的每次 chat 出站**必然携带**这 6 个头。
	// 我们此前一个都不发，属「官方发了、我们没发」的渠道特征。
	stainlessLang           = "js"
	stainlessPackageVersion = "6.25.0"
	stainlessOS             = "Windows"
	stainlessArch           = "x64"
	stainlessRuntime        = "node"

	// chatAcceptEncoding 官方桌面端主对话出站的 Accept-Encoding 取值。
	//
	// 取证（字节级 offset dump）：
	//   - 主对话链路是 sidecar → CLI → OpenAI Node SDK（fetch: axiosToFetchAdapter()）
	//     → axios 的 `_request`。axios 在 node adapter 里**强制写**该头：
	//       eo.set("Accept-Encoding", I.advertiseZstdAcceptEncoding===!0 ? tp : tu, !1)
	//     其中 tu="gzip, compress, deflate"+(hasBrotli?", br":"")，
	//     tp=tu+(hasZstd?", zstd":"")；advertiseZstdAcceptEncoding 默认 !1。
	//     Electron 内置 Node 的 zlib 自带 createBrotliDecompress（模块 43106 == 原生 zlib），
	//     故实际取值是 tu，即 "gzip, compress, deflate, br"。
	//
	// 关键纠错：此前的 `identity` 抄自 main/server.js 的 VlmUpstream.send()，
	// 那是**本地 VLM 技能代理**（127.0.0.1 + /skills/vlm/chat，入站头还要先过
	// 排除集 qN 丢掉 accept-encoding），与桌面端主对话不是同一条链路。发 identity
	// 属「我们发了、官方没发」的渠道特征。
	//
	// 副作用：Go 的 http.Transport 只在**它自己**添加该头时才透明解压；一旦我们显式
	// 设置，就必须在响应侧自行按 Content-Encoding 解码（见 decompressBody）。
	chatAcceptEncoding = "gzip, compress, deflate, br"

	// defaultStainlessRuntimeVersion 出站 X-Stainless-Runtime-Version。官方取运行时的
	// process.version（CLI 跑在 Electron 内置 Node 上），无法从 Go 侧推断，故内置一个
	// 与官方桌面端同代的取值作为默认，并留 config upstream.stainless_runtime_version
	// 覆盖口——线上校准无需求改码重编。
	defaultStainlessRuntimeVersion = "v22.14.0"

	// defaultStainlessTimeoutSeconds 出站 X-Stainless-Timeout（秒）。官方
	// buildOpenAIClientOptions 硬编码 `timeout: this.resolveRequestTimeoutMs()`，其默认
	// 返回 12e5 ms；SDK 用 String(Math.trunc(timeout/1e3)) 转秒 → 1200。
	// config upstream.stainless_timeout_seconds 覆盖。
	defaultStainlessTimeoutSeconds = "1200"
)

// uaAppNameFor 按账号 realm 返回 UA 第二段应用名（见 uaAppNameCN/uaAppNameGlobal）。
func uaAppNameFor(a *auth.Auth) string {
	if a != nil && a.IsGlobal() {
		return uaAppNameGlobal
	}
	return uaAppNameCN
}

// originRefererFor 按账号 realm 返回 Origin/Referer 基础域：
// global → https://www.workbuddy.ai；cn（含全局开关未开）→ https://www.codebuddy.cn。
func originRefererFor(a *auth.Auth) string {
	if a != nil && a.IsGlobal() {
		return originRefererGlobal
	}
	return originRefererCN
}

// defaultClientVersionFor 按账号 realm 返回内置客户端版本段：
// global → 5.8.4，cn（含 a 为 nil / 全局开关未开）→ 5.8.3。
func defaultClientVersionFor(a *auth.Auth) string {
	if a != nil && a.IsGlobal() {
		return defaultClientVersionGlobal
	}
	return defaultClientVersionCN
}

// clientVersion 生效的 WorkBuddy 客户端版本：Client.ClientVersion 非空则取之（用户
// 显式配置对两域统一生效），否则按账号 realm 取内置默认（见 defaultClientVersionFor）。
func (c *Client) clientVersion(a *auth.Auth) string {
	if c != nil && c.ClientVersion != "" {
		return c.ClientVersion
	}
	return defaultClientVersionFor(a)
}

// cliVersion 生效的 CLI 版本：Client.CliVersion 非空则取之，否则内置默认 defaultCliVersion。
func (c *Client) cliVersion() string {
	if c != nil && c.CliVersion != "" {
		return c.CliVersion
	}
	return defaultCliVersion
}

// stainlessRuntimeVersion 生效的 X-Stainless-Runtime-Version：Client 配置优先，否则内置默认。
func (c *Client) stainlessRuntimeVersion() string {
	if c != nil && c.StainlessRuntimeVersion != "" {
		return c.StainlessRuntimeVersion
	}
	return defaultStainlessRuntimeVersion
}

// stainlessTimeoutSeconds 生效的 X-Stainless-Timeout：Client 配置优先，否则内置默认。
func (c *Client) stainlessTimeoutSeconds() string {
	if c != nil && c.StainlessTimeoutSeconds != "" {
		return c.StainlessTimeoutSeconds
	}
	return defaultStainlessTimeoutSeconds
}

// defaultWorkBuddyUAFor 组装默认客户端出站 UA，逐段对齐官方国际版/国内版真实取值：
//
//	WorkBuddy/<clientVersion> <uaAppName>/<clientVersion> CLI/<cliVersion>
//
// 链路取证：桌面端主对话不经本地 VLM 代理（那是 /skills/vlm/chat 技能专用），而是
// main/sidecar-manager.js 起 CLI 子进程、注入 CLIENT_INFO_* 环境变量后由 CLI 直连
// /v2/chat/completions。CLI 的 UserAgentHttpInterceptor 会先删掉自带 UA 再重建：
//   - 第一段 platform：sidecar-manager.js 硬编码 `WorkBuddy`（var mt=`WorkBuddy`），两域同值；
//   - 第二段 applicationName：取自各自 product.json → CN `WorkBuddy`、global `workbuddy-ai`；
//   - 第三段 CLI/<ver>：来自 CLIENT_INFO_USER_AGENT_EXTENSION，值即内置 CLI 版本。
//
// 因此第二段必须按 realm 切。此前两域都写死 `WorkBuddy`，国内版因为 product.json 的
// applicationName 恰好也是 WorkBuddy 而看不出问题，国际版则与官方 UA 直接不符。
func (c *Client) defaultWorkBuddyUAFor(a *auth.Auth) string {
	v := c.clientVersion(a)
	return "WorkBuddy/" + v + " " + uaAppNameFor(a) + "/" + v + " CLI/" + c.cliVersion()
}

// userAgent 返回当前出站 UA（客户端出站路径：chat/refresh/FetchModels）。
// 优先级：Client.UserAgent（config user_agent）显式覆盖 > 按账号 realm 的默认 WorkBuddy 三段式
// （两域第二段相同，只有版本段随 realm 变）。
func (c *Client) userAgent(a *auth.Auth) string {
	if c != nil && c.UserAgent != "" {
		return c.UserAgent
	}
	return c.defaultWorkBuddyUAFor(a)
}

// billingUA 白名单类（billing/checkin/banner）出站 UA。
// 语义对齐官方 application-manifest.js:27590-27601（banner 白名单接口显式头组）：
// 这类接口用单段 `WorkBuddy/<clientVersion>`（不带 CLI 段——官方 banner 显式覆写 UA
// 为 `WorkBuddy/<pkgVer>`，RestOperations 层的 CLI 扩展段被业务层固化覆盖掉）。
// 默认（client_name 空）即生效（伪造官方桌面端指纹）；
// 显式 client_name="SaaS" 则不设 UA（还原旧行为，Go 默认 UA）。
// 版本段按账号 realm 取（CN 5.8.3 / global 5.8.4），与 chat 的 UA 保持同域一致。
func (c *Client) billingUA(a *auth.Auth) string {
	if c == nil || c.attributionClientName() == "SaaS" {
		return ""
	}
	return "WorkBuddy/" + c.clientVersion(a)
}

// resolveDeviceToken 解析本次请求的 X-Device-Token 取值。
// 优先级：auth.Auth.DeviceToken（每号）> Client.DeviceToken（config 全局）> 文件兜底。
// 三者皆空/读失败则返回空串（调用方不注入该头，优雅降级）。
// 为什么不放进 CommonHeaders：refresh 类请求给设备 token 无意义且可能被上游
// 风控误判为异常客户端；只在 billing 业务请求注入（chat 官方从不带，见 ChatHeaders）。
func (c *Client) resolveDeviceToken(a *auth.Auth) string {
	if a != nil && a.DeviceToken != "" {
		return a.DeviceToken
	}
	if c != nil && c.DeviceToken != "" {
		return c.DeviceToken
	}
	if c != nil && c.DeviceTokenFile != "" {
		return readDeviceTokenFile(c.DeviceTokenFile)
	}
	return ""
}

// injectDeviceToken 在 req 注入 X-Device-Token 头（仅当取到非空 token）。
func (c *Client) injectDeviceToken(req *http.Request, a *auth.Auth) {
	if tok := c.resolveDeviceToken(a); tok != "" {
		req.Header.Set("X-Device-Token", tok)
	}
}

// CommonHeaders 设置所有 API 共享的请求头。
func (c *Client) CommonHeaders(req *http.Request, a *auth.Auth) {
	req.Header.Set("Content-Type", "application/json")
	// Accept 非流式默认 application/json（D6：去掉宽松的 text/plain, */*）。
	// chat 路径在 ChatHeaders 覆盖为流式 event-stream。
	req.Header.Set("Accept", "application/json")
	origin := originRefererFor(a)
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")
	// User-Agent 三段式：第一段 platform 两域同值，第二段 applicationName 按 realm 切
	// （见 uaAppNameFor），第三段 CLI 版本。
	req.Header.Set("User-Agent", c.userAgent(a))
	// X-CodeBuddy-Request / X-Requested-With 保留在 CommonHeaders 供**非 chat** 路径使用
	// （/v3/config 与 models 列举的实测门禁依赖它们，见 global_models.go 注释）。
	// chat 路径**不**继承这两个头：ChatHeaders 末尾统一 Del。
	//   - X-CodeBuddy-Request 属于另一个客户端（CodeBuddy VS Code fork）。WorkBuddy AI
	//     全量包里它只出现在本地会话回放（buildSessionReplayRequestHeaders）、host 运行时
	//     探测（probeHostRuntimeEndpoint）与**本地 VLM 技能代理**（main/server.js 的
	//     applyGatewayHeaders，绑定 127.0.0.1 的 /skills/vlm/chat 路由）——桌面端主对话
	//     链路（sidecar → CLI → OpenAI SDK）上 grep 计数 0。
	//   - X-Requested-With 同样只出现在 VlmProxy 的 applyGatewayHeaders、web-fetch /
	//     web-search 头组与 Cell Framework 配置里，不在主对话出站。
	// 二者发到 chat 上游即「我们在发、官方主对话没发」的多余客户端指纹。
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("X-CodeBuddy-Request", "1")
	// Accept-Language 按 realm 切（D5）：CN zh-CN，global en-US。官方客户端按账号域
	// 发对应语言标识，对齐避免上游风控按语言缺失误判。
	req.Header.Set("Accept-Language", acceptLanguageFor(a))
	// 不再注入 X-Machine-ID / X-Session-ID：官方桌面端 5.8.4 全量分发包里这两个头
	// 一次都没出现过（grep 计数 0），自造的稳定派生值反而是最显眼的网关特征。
}

// acceptLanguageFor 按账号 realm 返回 Accept-Language：global → en-US，cn → zh-CN。
func acceptLanguageFor(a *auth.Auth) string {
	if a != nil && a.IsGlobal() {
		return "en-US"
	}
	return "zh-CN"
}

// injectCodeBuddyRequest 在 req 注入 X-CodeBuddy-Request: 1。
// billing 域未走 CommonHeaders，单独注入保证全出站覆盖。
func (c *Client) injectCodeBuddyRequest(req *http.Request) {
	req.Header.Set("X-CodeBuddy-Request", "1")
}

// injectGlobalChatHeaders global 账号的 chat 专属声明头，对齐官方国际版实测：
//   - X-Domain: www.workbuddy.ai  显式声明国际版域（官方多处以 auth.domain 注入）
//
// 不发 X-No-Enterprise-Id / X-No-Department-Info：官方全量分发包 grep 计数 0，
// chat 路径两者一律不发（见 ChatHeaders 末尾的统一 Del）。
//
// 仅 global realm 注入；CN 账号只在有值时发 X-Enterprise-Id / X-Domain。
func (c *Client) injectGlobalChatHeaders(req *http.Request, a *auth.Auth) {
	if a == nil || !a.IsGlobal() {
		return
	}
	req.Header.Set("X-Domain", "www.workbuddy.ai")
}

// ChatMeta 一次 chat 出站的会话头族元数据（issue #35：后台按 X-Conversation-Request-ID
// 聚合请求，官方客户端一次 user send 内所有 tool call/重试/换号复用同一个 ID）。
// handler 在轮转循环外生成 conversationID / conversationRequestID，循环内每次出站
// 复用同值；TraceID 透传入站值（空 = 回落 conversationRequestID）。
// messageID（消息级，每条独立）由 ChatHeaders 内部生成，无需外部可见。
type ChatMeta struct {
	ConversationID        string // X-Conversation-ID：body 提取的入站值，空则不发（透传优先，不伪造）
	ConversationRequestID string // X-Conversation-Request-ID：聚合主键，必发
	TraceID               string // X-Trace-ID：入站透传值，空则回落 conversationRequestID
}

// ChatHeaders 在 common 之上加 chat 专属的账号头。
// 缺省字段用 X-No-* 约定（与 CodeBuddy 官方 CLI 一致）。
// clientIP 为本次请求的客户端 IP（按参数传递，不读共享字段——避免并发串扰）；
// PassthroughIP=false 或 clientIP 为空时不注入 IP 头。
// meta 为会话头族元数据（CN/global 同构，纯新增，不改既有头），见 injectConversationHeaders。
func (c *Client) ChatHeaders(req *http.Request, a *auth.Auth, clientIP string, meta ChatMeta) {
	c.CommonHeaders(req, a)
	// Accept 不覆盖。官方 SDK 的 buildHeaders 固定 `Accept: application/json`
	// （3043.50ee4dca.mjs），而 SDK 的请求体只带 body+headers，不回写 Accept；
	// 本地 VLM 技能代理的 applyGatewayHeaders 同样是 application/json。
	// 流式与否由 body 的 `stream:true` 决定、响应照常是 SSE。早先覆盖成
	// `application/json, text/event-stream` 是自造形态（官方只有 ACP transport 的
	// buildAcpHeaders 用这个值，那是本地 ACP 连接、不是 chat 上游）。
	// AccessToken 加锁快照：keepalive 定时刷新会在 a.mu 内改写它，锁外直读构成数据竞争
	// （见 auth.AccessTokenValue 注释与 upstream.TestChatHeadersRacesRefreshToken）。
	// 无凭据时不发 X-No-Authorization / X-No-User-Id：官方分发包 grep 计数 0，
	// 缺省就是「不带这个头」，X-No-* 声明反而是额外信号。
	if at := a.AccessTokenValue(); at != "" {
		req.Header.Set("Authorization", "Bearer "+at)
	}
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	// 安全红线：绝不在 chat 请求里携带 X-Refresh-Token。
	// 企业与域头按 realm 分发：CN 只在有值时透传 EnterpriseID/Domain；global 账号
	// 由 injectGlobalChatHeaders 覆写为国际客户端形态（X-Domain=www.workbuddy.ai），
	// 且不回退 X-Domain 到登录会话原值。
	// X-No-* 缺省头两域都不发——官方全量分发包 grep 计数 0（2026-10-19 复验），
	// 发出去是纯粹的额外信号；末尾统一 Del 兜底。
	if a != nil && !a.IsGlobal() {
		if a.EnterpriseID != "" {
			req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
		}
		if d := a.DomainValue(); d != "" {
			req.Header.Set("X-Domain", d)
		}
	} else {
		c.injectGlobalChatHeaders(req, a)
	}
	// 用量归属头：真实桌面端发 X-Agent-Purpose="conversation" + X-IDE-Name/Type/X-Product
	// 识别 client，避免上游用量统计里 client/agentPurpose 为空。来源 xiaofan6ya/converter.py。
	// 默认（ClientName 空）即伪造 WorkBuddy 桌面端头组（见 injectAttribution）；
	// 显式 ClientName="SaaS" 还原旧行为（仅 X-Product="SaaS"）。
	c.injectAttribution(req, a)
	// 客户端 IP 透传：仅当 PassthroughIP=true 且本次请求 clientIP 参数非空（见 handler 设置）。
	// 缺省 false（反代安全边界：不把内网/代理 IP 暴露给上游）。
	c.injectClientIP(req, clientIP)
	// 会话头族（对话/请求/消息/B3 链路）：纯新增，CN/global 同构，见 injectConversationHeaders。
	c.injectConversationHeaders(req, meta)
	// 官方桌面端主对话（CLI → OpenAI SDK → axios）不发的头，这里显式抹掉：
	//   - Origin：VlmProxy 入站排除集 qN 里就有 `origin`；主对话 axios 层也不带；
	//   - Referer / Accept-Language：全量分发包 main/ 下 grep 计数 0（Accept-Language
	//     只出现在 teams 拦截器，取值 zh-CN/en-US）；axios 层也不加；
	//   - X-No-Enterprise-Id / X-No-Department-Info：grep 计数 0（官方缺省就不发该头）；
	//   - X-Device-Token：官方只在 checkin / auth 类请求（buildHeadersWithTuringToken）
	//     注入，chat 从不带（见 account.js getCheckinRequestHeaders）。
	// 这些都是"我们在发、官方没发"的额外信号，chat 路径上直接对齐官方形态。
	for _, h := range []string{
		"Origin",
		"Referer",
		"Accept-Language",
		"X-No-Enterprise-Id",
		"X-No-Department-Info",
		"X-Device-Token",
		// 另一客户端（CodeBuddy）与浏览器侧的闸门头：CommonHeaders 已不再注入，
		// 此处 Del 兜底防未来回归（见 CommonHeaders 注释）。
		"X-CodeBuddy-Request",
		"X-Requested-With",
	} {
		req.Header.Del(h)
	}
	// Accept-Encoding 对齐官方桌面端主对话的 axios 层（见 chatAcceptEncoding 注释）。
	// 注意：一旦我们显式设置该头，Go 的 http.Transport 就不再自动加 gzip 并透明解压，
	// 因此响应侧必须自己解（见 decompressBody）。
	req.Header.Set("Accept-Encoding", chatAcceptEncoding)
	// OpenAI SDK 指纹族（官方 chat 走 SDK，默认必发；见 injectStainlessHeaders 注释）。
	c.injectStainlessHeaders(req)
}

// injectStainlessHeaders 复刻官方 OpenAI Node SDK buildHeaders 发出的 X-Stainless-* 族。
//
// 取证路径（字节级 offset dump，非推测）：
//   - resources/app.asar.unpacked/cli/dist/codebuddy-lite-wb.mjs 的
//     buildOpenAIClientOptions 为 chat 创建客户端：
//       {baseURL, apiKey, fetch: this.axiosToFetchAdapter(), maxRetries:0, timeout:resolveRequestTimeoutMs()}
//     resolveRequestTimeoutMs 默认返回 12e5（ms）。
//   - 同包 lazy-lite-wb/3043.50ee4dca.mjs 的 buildHeaders 无条件拼入：
//       Accept / "User-Agent": getUserAgent() / X-Stainless-Retry-Count / [X-Stainless-Timeout] /
//       {X-Stainless-Lang, Package-Version, OS, Arch, Runtime, Runtime-Version} /
//       OpenAI-Organization / OpenAI-Project
//   - 该族只在 er 分支（CODEBUDDY_SKIP_INTERNAL_HEADERS 为真）由 axiosToFetchAdapter
//     的 transformRequest 按 `x-stainless-` 前缀剥除；该 env **默认不设**。
//
// 不设 OpenAI-Organization / OpenAI-Project：构造参数默认 readEnv(...) ?? null，
// 官方未配 OPENAI_ORG_ID/OPENAI_PROJECT_ID，SDK 的 buildHeaders 对 null 值做 delete，
// 实际上线不带——我们带了反而是多出项。
func (c *Client) injectStainlessHeaders(req *http.Request) {
	req.Header.Set("X-Stainless-Retry-Count", "0")
	req.Header.Set("X-Stainless-Timeout", c.stainlessTimeoutSeconds())
	req.Header.Set("X-Stainless-Lang", stainlessLang)
	req.Header.Set("X-Stainless-Package-Version", stainlessPackageVersion)
	req.Header.Set("X-Stainless-OS", stainlessOS)
	req.Header.Set("X-Stainless-Arch", stainlessArch)
	req.Header.Set("X-Stainless-Runtime", stainlessRuntime)
	req.Header.Set("X-Stainless-Runtime-Version", c.stainlessRuntimeVersion())
}

// injectConversationHeaders 注入官方客户端会话头族（issue #35 后台聚合）。
// 头族分四层，各司其职：
//   - X-Conversation-ID：会话级，多轮稳定（body 的 conversationId）。空则不发——
//     透传客户端原值优先，客户端没给就不伪造，避免误导后台建错会话。
//   - X-Conversation-Request-ID：**对话轮级聚合主键**，必发。一次 user send 内的
//     所有 tool call/重试/换号/降级复用同一个 → 后台按它聚合成一条（不再碎片化）；
//     换 user 消息即换键（#170 统一轮级，对齐官方 CLI 的 USER_PROMPT_SUBMIT 重生成
//     语义；调用方 handler 负责保证轮级键的生成，透传客户端值优先）。
//   - X-Conversation-Message-ID = X-Request-ID：消息级，每条独立（32 位 hex）。
//   - X-Trace-ID：入站透传或 = conversationRequestID。
//   - X-B3-TraceId / X-B3-SpanId / X-B3-ParentSpanId / X-B3-Sampled：链路族。B3 规范
//     只认 16/32 hex TraceId 与 16 hex SpanId；入站 conversationRequestID 非法时
//     TraceId 回落 messageID（恒 32 hex），SpanId 取 messageID[:16]、ParentSpanId
//     取 messageID[16:]（每消息新）。
//   - traceparent / b3：W3C 与 B3 单头形式，对齐官方 applyTraceHeaders 的四件套。
//     官方不发 X-Root-Request-ID（全量分发包 grep 计数 0），本地也不发。
func (c *Client) injectConversationHeaders(req *http.Request, meta ChatMeta) {
	convReqID := meta.ConversationRequestID
	if convReqID == "" {
		// 零值 meta（直接调 ChatHeaders 的调用方/测试）也要保证聚合主键必发：
		// 本级补一个 32 hex，调用方（handler）已生成稳定的不走到这里。
		convReqID = session.NewMessageID()
	}
	messageID := session.NewMessageID()
	if meta.ConversationID != "" {
		req.Header.Set("X-Conversation-ID", meta.ConversationID)
	}
	req.Header.Set("X-Conversation-Request-ID", convReqID)
	req.Header.Set("X-Conversation-Message-ID", messageID)
	req.Header.Set("X-Request-ID", messageID)
	traceID := meta.TraceID
	if traceID == "" {
		traceID = convReqID
	}
	req.Header.Set("X-Trace-ID", traceID)
	b3Trace := convReqID
	if !validTraceID(b3Trace) {
		b3Trace = messageID // 非法 B3 TraceId → 回落恒 32 hex 的消息级 ID
	}
	spanID := messageID[:16]
	parentSpanID := messageID[16:]
	req.Header.Set("X-B3-TraceId", b3Trace)
	req.Header.Set("X-B3-SpanId", spanID)
	req.Header.Set("X-B3-ParentSpanId", parentSpanID)
	req.Header.Set("X-B3-Sampled", "1")
	// traceparent 的 trace-id 规范只认 32 hex（W3C 无 16 hex 简写），而 B3 允许
	// 16/32：入站透传的 16 hex conversationRequestID 在 B3 里合法、在 traceparent
	// 里不合法，这时 traceparent 单独用消息级 32 hex。
	tpTrace := b3Trace
	if len(tpTrace) != 32 {
		tpTrace = messageID
	}
	req.Header.Set("traceparent", "00-"+tpTrace+"-"+spanID+"-01")
	req.Header.Set("b3", b3Trace+"-"+spanID+"-1-"+parentSpanID)
}

// validTraceID 判断 B3 TraceId 是否合法：16 或 32 位 hex（全新大小写均可）。
// 官方客户端生成的 conversationRequestId 是 32 位 hex（UUID 去横线），入站透传值
// 可能是任意形状（含横线/超长/非 hex），直接塞进 B3 头会破坏链路关联（issue #35）。
func validTraceID(s string) bool {
	if len(s) != 16 && len(s) != 32 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

// attributionClientName 生效的用量归属名：ClientName 非空取之；
// 空默认 "WorkBuddy"（伪造官方桌面端指纹；显式配 "SaaS" 可还原旧行为）。
func (c *Client) attributionClientName() string {
	if c != nil && c.ClientName != "" {
		return c.ClientName
	}
	return "WorkBuddy"
}

// injectAttribution 注入用量归属头（X-Agent-Intent / X-Agent-Purpose / X-IDE-* /
// X-Agent-Type / X-Private-Data）。仅在 chat/completions 路径生效（ChatHeaders 调用）。
//
// 头名与取值逐项对齐官方国际版客户端（D:\PG\workbuddyAI 取证，2026-10-19）：
//   - CLI 侧 `axiosToFetchAdapter`（dist/codebuddy-lite-wb.mjs）对每条模型请求设
//     X-Agent-Intent / X-Agent-Purpose / X-IDE-Type / X-IDE-Name / X-IDE-Version /
//     X-Agent-Type；渲染层 `applyGatewayHeaders` + `YN` 默认表再补 X-Private-Data。
//   - X-Agent-Intent=craft      官方唯一取值为会话 meta["codebuddy.ai/mode"] ?? "craft"
//   - X-Agent-Purpose=conversation  官方 getEffectiveAgentPurpose 主对话取值
//   - X-Agent-Type=main         resolveSessionAgentType：无 teamContext/parentConversationId → "main"
//   - X-Private-Data=true       官方默认表恒设
//
// **不发 X-Product**：官方 CLI 的 yc.PRODUCT 常量只有声明、全包无赋值点，渲染层
// YN 默认表仅在 inbound 已带该头时兜底（e.get(n)||r），而 CLI→网关这条链上没人设。
// 实测国际版出站头集里没有 X-Product，我们之前发 `X-Product: WorkBuddy` 属多出项。
func (c *Client) injectAttribution(req *http.Request, a *auth.Auth) {
	name := c.attributionClientName()
	if name == "SaaS" {
		// 旧行为兜底：仅显式配置 ClientName="SaaS" 时还原（也是 X-Product 唯一发出场景）
		req.Header.Set("X-Product", "SaaS")
		return
	}
	req.Header.Set("X-Agent-Intent", "craft")
	req.Header.Set("X-Agent-Purpose", "conversation")
	req.Header.Set("X-Agent-Type", "main")
	req.Header.Set("X-Private-Data", "true")
	req.Header.Set("X-IDE-Name", name)
	req.Header.Set("X-IDE-Type", name)
	req.Header.Set("X-IDE-Version", c.clientVersion(a))
}

// injectClientIP 在 PassthroughIP 开启时把 clientIP 参数透传给上游。
// 三个等价头（X-Forwarded-For/X-Real-IP/X-Client-IP）一并设，与桌面端透传一致。
// 按**参数传递**而非读共享字段：避免并发请求交叉污染对方 IP（issue：ClientIP 竞态）。
func (c *Client) injectClientIP(req *http.Request, clientIP string) {
	if c == nil || !c.PassthroughIP || clientIP == "" {
		return
	}
	req.Header.Set("X-Forwarded-For", clientIP)
	req.Header.Set("X-Real-IP", clientIP)
	req.Header.Set("X-Client-IP", clientIP)
}

// ExtractClientIP 从入站请求提取客户端 IP 首段（X-Forwarded-For 首段，回落 X-Real-IP）。
// 供 handler 在 PassthroughIP 开启时按请求取值后传入 ChatStream（chat 路径专属，不跨请求）。
// 取不到返回空串（handler 据此跳过透传）。
func ExtractClientIP(r *http.Request) string {
	if r == nil {
		return ""
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		for i := 0; i < len(xff); i++ {
			// 取逗号前首段并 trim 空白。
			if xff[i] == ',' {
				return strings.TrimSpace(xff[:i])
			}
		}
		return strings.TrimSpace(xff)
	}
	if real := strings.TrimSpace(r.Header.Get("X-Real-IP")); real != "" {
		return real
	}
	return ""
}

// BillingHeaders billing 接口请求头。
// UA 语义（对齐官方白名单头组，application-manifest.js:27590-27601）：
//  1. 显式配置 c.UserAgent 优先（用户自定义值，全路径生效）；
//  2. 未配且归属名非 SaaS（含默认 WorkBuddy）→ 单段 `WorkBuddy/<clientVersion>`
//     （官方 banner/check-in 显式覆写 UA 的形态，不带 CLI 段）；
//  3. 显式 client_name="SaaS" → 不设置（Go 客户端自带默认 UA，还原旧行为）。
func (c *Client) BillingHeaders(req *http.Request, a *auth.Auth) {
	// AccessToken 加锁快照（同 ChatHeaders：keepalive 可在 a.mu 内改写）。
	req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	c.injectCodeBuddyRequest(req)
	// Accept-Language 按 realm 切（D5）：billing 域未走 CommonHeaders，单独注入。
	req.Header.Set("Accept-Language", acceptLanguageFor(a))
	if c != nil && c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	} else if ua := c.billingUA(a); ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	if a.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
		req.Header.Set("X-Tenant-Id", a.EnterpriseID)
	}
	if d := a.DomainValue(); d != "" {
		req.Header.Set("X-Domain", d)
	}
	// 设备风控头：billing 域（report/travel/balance/checkin）同样注入（见 resolveDeviceToken）。
	c.injectDeviceToken(req, a)
}

// RefreshHeaders refresh 端点专属头（X-Refresh-Token 只允许出现在这里）。
func (c *Client) RefreshHeaders(req *http.Request, a *auth.Auth) {
	c.CommonHeaders(req, a)
	req.Header.Set("X-Refresh-Token", a.RefreshToken)
	if a.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
	}
	// X-Auth-Refresh-Source 对齐官方客户端 refresh 渠道标识 "plugin"（D3）。
	req.Header.Set("X-Auth-Refresh-Source", "plugin")
}
