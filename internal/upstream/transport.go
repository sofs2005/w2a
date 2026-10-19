// transport.go 出站 Transport 构造的单一事实来源（连接层加固，吸收 kongjianguan
// 4 连击实测经验的前三件，见 .claude/reports/fork-scan-absorb.md T-2）：
// 真正禁 h2 / TLS 握手超时 / 短 keepalive 探测，全参数集中定义可测试可调整。
// 第四件 DisableKeepAlives 按报告 trade-off 不吸收（每请求 TLS 握手开销与连接
// 复用意图相反），分析见 .claude/reports/transport-hardening.md。
package upstream

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// 连接层参数集中定义（与 server/backoff.go 同风格：一处定义，测试可回读断言）。
const (
	// dialTimeout TCP 连接建立上限。半死连接的第一道闸：连不上就快速失败
	// 轮转换号，不再干等系统 TCP 重传窗口（kongjianguan 实测半开 TCP 单次
	// TTFB 卡 936s——默认 Dialer 无超时上限）。
	dialTimeout = 10 * time.Second
	// dialKeepAlive TCP keepalive 探测周期。默认 Dialer 2h 才发首个探测——
	// NAT 黑洞里 2h 足够连接半死且被复用。15s 周期让死连接在 15~30s 内被
	// 内核掐掉（RST/ETIMEDOUT），复用侧立即感知而非卡到重传窗口。
	dialKeepAlive = 15 * time.Second
	// tlsHandshakeTimeout TLS 握手上限。现役此前完全缺失——握手挂起时无任何
	// 层兜底（ResponseHeaderTimeout 只在请求写完后才计时），只能干等到
	// HTTP.Client.Timeout(120s)。
	tlsHandshakeTimeout = 10 * time.Second
	// idleConnTimeout 空闲连接池保留时长。从 90s 收到 30s：WAF 风暴后上游
	// NGI 常态性掐闲置连接，90s 池里的连接多半已死（kongjianguan 同款取值）；
	// 复用侧仍有 15s keepalive 兜底识别。
	idleConnTimeout = 30 * time.Second
	// responseHeaderTimeout 聊天 SSE 首字节前（响应头）硬上限。从 120s 收到
	// 60s：kongjianguan 笔记实录成功请求 TTFB 曾到 16s（慢模型冷启动），
	// 60s ≈ 3.75× 观测最坏健康首包，留足慢冷启动余量；同时把半死连接场景的
	// 单请求卡死从 2 分钟压到 1 分钟（MaxRotate 默认 3 次的最坏轮转从 6 分钟
	// 压到 3 分钟）。不取 kongjianguan 的 20s：其 20s 是 DisableKeepAlives+
	// timedConn 20s 写超时组合的取值，我们保留连接复用，须按自身慢冷启动
	// 观测留余量。语义核对（任务书设计纪律）：ResponseHeaderTimeout 只计响应
	// 头到达前的时长，头到达后 SSE 长流不受影响（流中空闲由 IdleTimeout 监控，
	// 见 idle.go），不误杀长流——transport_test.go 有显式回归。
	//
	// 注意：本常量只是 newTransport 的构造默认，main.go 会按 config
	// upstream.header_timeout_seconds 无条件覆盖。因此生产生效值 = config
	// 解析值（未配置时 normalize 回落 timeout_seconds，默认 120），本 60s 仅作
	// 「Config 未接线/测试裸用」时的安全网——与 config.example.json 的取值
	// 对齐避免三处口径漂移（transport 60 / config 回落 120 / example 60）。
	responseHeaderTimeout = 60 * time.Second
)

// maxIdleConns / maxIdleConnsPerHost 连接池容量（既有值，一并集中定义）。
const (
	maxIdleConns        = 100
	maxIdleConnsPerHost = 20
)

// newDialer 构造出站拨号器（DialContext 的 Timeout/KeepAlive 参数集中于此，
// 供测试回读断言）。
func newDialer() *net.Dialer {
	return &net.Dialer{
		Timeout:   dialTimeout,
		KeepAlive: dialKeepAlive,
	}
}

// newTransport 构造共享出站 Transport（HTTP 与 ChatHTTP 同一实例，连接池不重复）。
// 分两层防半死连接：
//   - TLS 层：空 TLSNextProto 真正禁 h2（kongjianguan 二次修正的实证：ForceAttemptHTTP2=false
//     只对自定义 Dial 生效，默认 TLS 经 ALPN 仍协商出 h2，半死 h2 流复用表现为
//     "http2: timeout awaiting response headers"——唯一正确写法是置空映射，让 ALPN
//     完成后无 h2 协议可用，连接退回 HTTP/1.1）。
//   - TCP 层：DialContext 10s 建连上限 + 15s keepalive 探测，半开连接在建立期
//     和复用期都能被快速识别（见 dialTimeout/dialKeepAlive 注释）。
//
// 出站代理：Proxy 取 http.ProxyFromEnvironment，读标准环境变量 HTTP_PROXY /
// HTTPS_PROXY / NO_PROXY（含小写形态）。这是**刻意**的选择——本网关常在受限网络里
// 部署（无直连、须经企业代理出网），而此前完全没有代理能力：连不上上游时只能表现为
// 「所有账号都不可用」，排查方向上会一路跑偏到账号/风控，实际根因是网络不可达。
//
// 三点设计约束：
//   - 环境变量优先于代码默认：标准库 ProxyFromEnvironment 在变量未设置时返回 nil
//     （= 直连），故对「不设变量」的既有部署是零行为变化，无需开关。
//   - NO_PROXY 由标准库一并处理（含 "example.com"、".example.com"、"*" 等形态），
//     不自造匹配逻辑——自造版本几乎必然漏掉某条标准形态。
//   - 本函数在启动时构造一次；而 http.ProxyFromEnvironment **自身**也把环境变量
//     读进进程级 sync.Once 缓存（进程内只解析一次）。两者叠加的语义是：启动后改
//     环境变量不生效，**改代理须重启进程**——与其它出站参数（超时等）同口径。
//     这既是标准库的既定行为，也是本网关「配置改动需重启」的既有约定（见
//     config 里各 restart 注记），不另造热加载。
//
// 按域分档（proxy.cn / proxy.global，见 proxy.go）：CN 侧（本函数）保持不变；
// global 侧由 newTransportGlobal 在**本函数的产物**上覆盖 Proxy/DialContext，
// 连接层加固参数一个不丢。显式配置优先于环境变量，未配置则回落本函数的环境变量
// 语义——故「不配 proxy 段」的部署与历史行为逐字节一致。
func newTransport() *http.Transport {
	dialer := newDialer()
	return &http.Transport{
		DialContext: dialer.DialContext,
		// 环境变量代理（HTTP_PROXY/HTTPS_PROXY/NO_PROXY）；未设置 = 直连（零回归）。
		Proxy: http.ProxyFromEnvironment,
		// 空 TLSNextProto（非 nil）真正禁 h2：见函数注释。必须 make 而非 nil——
		// nil 表示「让标准库注入默认 h2 映射」（kongjianguan 实测：设
		// ForceAttemptHTTP2=false 后日志仍报 h2 timeout，正是这个陷阱）。
		TLSNextProto: make(map[string]func(authority string, c *tls.Conn) http.RoundTripper),
		TLSHandshakeTimeout:   tlsHandshakeTimeout,
		MaxIdleConns:          maxIdleConns,
		MaxIdleConnsPerHost:   maxIdleConnsPerHost,
		IdleConnTimeout:       idleConnTimeout,
		ResponseHeaderTimeout: responseHeaderTimeout,
	}
}

// newTransportGlobal 构造 global 域专用 Transport（按域分代理的 global 侧）。
//
// proxyURL 语义：
//   - 空 / 非法 → 与 newTransport() 完全一致（回落 HTTP_PROXY/HTTPS_PROXY 环境
//     变量，再空则直连）。非法值静默回落是**刻意**的：config.normalize 已在启动
//     时校验并 WARN 过，运行期再报一次只会重复噪音。
//   - 合法 → 在加固后的 Transport 上接入该代理（见 proxy.go applyProxy）。
//
// 为什么要独立 Transport 而非「一个 Transport + 按 req 动态选代理」：连接池隔离。
// 代理路径与直连路径的 RTT、半死概率、TLS 指纹完全不同，混在一个池里会让
// roundTripCloseIdle 的清池误伤另一条腿；且 ResponseHeaderTimeout 等参数无法
// 按域调优。代价是连接池数量翻倍（MaxIdleConns=100 上限，可接受）。
func newTransportGlobal(proxyURL string) *http.Transport {
	tr := newTransport()
	u, err := ParseProxyURL(proxyURL)
	if err != nil || u == nil {
		return tr // 空 = 环境变量语义；非法 = 已 WARN 过，回落
	}
	if err := applyProxy(tr, u); err != nil {
		return newTransport() // 理论上不可达（scheme 已过白名单），保险起见回落
	}
	return tr
}

// DirectRouteName 账号级线路表的保留名：显式直连（忽略 HTTP_PROXY 等环境变量）。
//
// 为什么需要它：线路表里的其他名字都是「经某代理出去」，但有些号恰恰要裸出口
// （宿主 IP 才是它注册时的 IP，走代理反而像异地登录）。若没有这个保留名，用户
// 只能不写 proxy，而那等于「跟随进程级环境代理」—— 环境里有 HTTP_PROXY 时无法
// 表达「这个号必须直连」。保留名不参与 ConfigureProxies 的表项。
const DirectRouteName = "__direct__"

// ── 账号级出口线路（issue #210 / 面板「出口线路」）───────────────────────────
//
// 面板早就能给账号绑一条命名线路（`auths/*.json` 顶层 `proxy` 字段，值是**线路名**），
// 但上游此前完全不读它：出站只有 ProxyFromEnvironment（进程级），于是所有账号共用
// 宿主出口 IP —— 界面上看起来已经隔离，实际没有。多号同 IP 正是平台最容易关联的
// 特征，所以这里按线路名构造并**分开缓存** Transport（连接池必须分开：共用一份会把
// A 线路的空闲连接复用给 B 线路，出口照样会串）。
//
// 线路表来自 `config.json` 的 `proxies` 段（线路名 → 代理地址）。线路名查不到时
// **明确报错、不静默直连** —— 静默直连会给出「以为隔离了其实没隔离」这个最坏结果。
var routes = struct {
	mu    sync.RWMutex
	table map[string]string
	cache map[string]*http.Transport
}{table: map[string]string{}, cache: map[string]*http.Transport{}}

// routeHeaderTimeout 线路 Transport 的 ResponseHeaderTimeout，由 main 在
// ConfigureProxies 之前注入（Transport 是惰性构造的，构造时读取该值）。
// 未注入（测试裸用）时为 0 = 沿用 newTransport 的构造默认。
var routeHeaderTimeout atomic.Int64

// SetRouteHeaderTimeout 设置线路 Transport 的响应头超时（<=0 不覆盖构造默认）。
// 必须在 ConfigureProxies **之前**调用，且此后不再修改（Transport 只有创建时读一次）。
func SetRouteHeaderTimeout(d time.Duration) {
	routeHeaderTimeout.Store(int64(d))
}

// ConfigureProxies 注入「线路名 → 代理地址」表（main 启动时调用一次；传 nil 清空）。
func ConfigureProxies(table map[string]string) {
	routes.mu.Lock()
	defer routes.mu.Unlock()
	routes.table = map[string]string{}
	// 清缓存：地址改了必须重建连接池，否则旧代理连接会继续用
	routes.cache = map[string]*http.Transport{}
	for name, addr := range table {
		n := strings.TrimSpace(name)
		v := strings.TrimSpace(addr)
		// 保留名不参与线路表（它的含义由 TransportFor 内建）
		if n == "" || v == "" || n == DirectRouteName {
			continue
		}
		routes.table[n] = v
	}
}

// RouteNames 已配置的线路名（升序；供启动日志与排障）。
func RouteNames() []string {
	routes.mu.RLock()
	defer routes.mu.RUnlock()
	out := make([]string, 0, len(routes.table))
	for name := range routes.table {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// directTransport 显式直连专用 Transport（Proxy=nil，不看环境变量）。
// sync.OnceValue 保证全进程一份：连接池不分裂，且不会每次取线路都新建连接池。
var directTransport = sync.OnceValue(func() *http.Transport {
	tr := newTransport()
	tr.Proxy = nil
	if d := routeHeaderTimeout.Load(); d > 0 {
		tr.ResponseHeaderTimeout = time.Duration(d)
	}
	return tr
})

// normalizeProxyURL 归一化代理地址：`socks5h://` 与 `socks5://` 等价（Go 的 socks5
// 拨号本身把域名交给代理解析），统一成 `socks5://`；其余 scheme 原样。
func normalizeProxyURL(raw string) (*url.URL, error) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return nil, fmt.Errorf("empty proxy address")
	}
	u, err := url.Parse(v)
	if err != nil {
		return nil, fmt.Errorf("invalid proxy address: %w", err)
	}
	switch strings.ToLower(u.Scheme) {
	case "socks5h":
		u.Scheme = "socks5"
	case "socks5", "http", "https":
	default:
		return nil, fmt.Errorf("unsupported proxy scheme %q (want http/https/socks5/socks5h)", u.Scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("proxy address has no host: %q", v)
	}
	return u, nil
}

// TransportFor 返回线路名对应的出站 Transport：
//   - 空名 → 默认（进程级环境代理，见 newTransport）
//   - DirectRouteName → 显式直连（Proxy=nil，忽略环境变量）
//   - 已配置 → 该线路专属 Transport（按名缓存，连接池与默认分开）
//   - 未配置 → error（不静默直连；错误里带线路名，便于对照 config.json）
func TransportFor(name string) (*http.Transport, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return newTransport(), nil
	}
	if name == DirectRouteName {
		return directTransport(), nil
	}
	routes.mu.RLock()
	addr, ok := routes.table[name]
	cached := routes.cache[name]
	known := len(routes.table)
	routes.mu.RUnlock()
	if !ok {
		if known == 0 {
			return nil, fmt.Errorf("账号绑定了出口线路 %q，但上游没有配置任何线路"+
				"（config.json 的 proxies 段）", name)
		}
		return nil, fmt.Errorf("未知的出口线路 %q（config.json 的 proxies 里没有它）", name)
	}
	if cached != nil {
		return cached, nil
	}
	u, err := normalizeProxyURL(addr)
	if err != nil {
		return nil, fmt.Errorf("出口线路 %q 的地址无效：%w", name, err)
	}
	tr := newTransport()
	tr.Proxy = http.ProxyURL(u)
	if d := routeHeaderTimeout.Load(); d > 0 {
		tr.ResponseHeaderTimeout = time.Duration(d)
	}
	routes.mu.Lock()
	if routes.cache == nil {
		routes.cache = map[string]*http.Transport{}
	}
	// 双检：并发首次创建时只保留一份（否则两个 goroutine 各建一个池，出口仍会串）
	if again := routes.cache[name]; again != nil {
		routes.mu.Unlock()
		return again, nil
	}
	routes.cache[name] = tr
	routes.mu.Unlock()
	return tr, nil
}

// closeIdler 实现该接口的 RoundTripper 支持清空空闲连接池（*http.Transport、
// http2.Transport 等均满足；测试注入的自定义 RoundTripper 可选择性实现）。
type closeIdler interface {
	CloseIdleConnections()
}

// roundTripCloseIdle 在传输层请求失败后清掉 rt 所属 Transport 的空闲连接池
// （kongjianguan 第 4 件：失败连接可能仍留在空闲池里，等 IdleConnTimeout 才
// 过期，下一个请求会继续捡到它）。
//
// 挂载点（任务书「评估挂载点：错误分类处理处」的结论）：错误分类（Classify）
// 只见业务信封——传输层失败根本没有 body 可分类（见 doJSON/ChatStreamContext
// 对 read body 失败的处理：不进 Classify、不罚号）。这类失败的正确处理正是
// 连接层的池清理，故挂在与 Do 并列的传输层出口（ChatStreamContext 的 Do 错误
// 分支），而非 applyErrorPolicy。
//
// 关闭是 best-effort：rt 为 nil 或未实现 closeIdler（如测试注入的 rtFunc）时
// 静默跳过。CloseIdleConnections 只关空闲连接，不影响在途请求；瞬时代价是
// 下个请求多一次 TCP+TLS 握手，与半死连接被复用卡 60s 的风险完全不成比例。
func roundTripCloseIdle(rt http.RoundTripper) {
	if rt == nil {
		return
	}
	if ci, ok := rt.(closeIdler); ok {
		ci.CloseIdleConnections()
	}
}
