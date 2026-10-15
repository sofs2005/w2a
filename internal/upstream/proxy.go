// proxy.go 出站代理的解析与接线（按域分档：proxy.cn / proxy.global）。
//
// 与 transport.go 的关系：transport.go 负责连接层加固参数的集中定义，本文件负责
// 「代理怎么接上去」。两者共用 newTransport()——代理版本是在加固后的 Transport 上
// 覆盖 Proxy / DialContext 两个字段，加固参数一个不丢。
//
// 支持的 scheme（白名单，normalize 与运行期共用同一份判定）：
//   - http / https：走标准库 http.ProxyURL（CONNECT 隧道；https = 代理本身走 TLS，
//     Go 1.20+ 原生支持）。userinfo 由标准库转成 Proxy-Authorization: Basic。
//   - socks5 / socks5h：走 golang.org/x/net/proxy.SOCKS5 包 DialContext。
//
// ⚠️ socks5 与 socks5h 在本实现里行为**相同**（都是代理解析域名）：x/net 的
// socks.Dialer 在 connect 阶段对非 IP 的 host 一律发 AddrTypeFQDN（见其
// internal/socks/client.go 的 net.ParseIP 分支），没有「本地解析后再连」的路径。
// 两个 scheme 都接受是为了兼容用户的书写习惯（curl 里 socks5 是本地解析、
// socks5h 是代理解析），本网关统一按代理解析——国内部署访问国外域名时这更可靠。
//
// 与环境变量的关系（fallback，见 newTransportGlobal）：显式配置优先，未配置回落
// http.ProxyFromEnvironment，两者都空 = 直连。显式配置**不**叠加 NO_PROXY——给了
// 某域的代理就是「这域走这个代理」，再叠一层排除规则会让「到底走没走代理」变成
// 不可预测的状态。
package upstream

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"golang.org/x/net/proxy"
)

// proxySchemes 允许的代理 scheme（小写）。normalize 与 applyProxy 共用。
var proxySchemes = map[string]bool{
	"http":    true,
	"https":   true,
	"socks5":  true,
	"socks5h": true,
}

// ParseProxyURL 解析并校验代理 URL 字符串。
//
// 返回值语义（三态，调用方据此决定行为）：
//   - ("", nil, nil)：空串（含纯空白）= 该域未配置代理 → 调用方回落环境变量。
//   - (u, nil)：合法代理 URL。
//   - ("", err)：非法 → 调用方应 WARN 并回落直连（normalize 口径：不 fail-fast，
//     一个错字段不该让网关起不来）。
//
// 校验规则：scheme 在白名单内、url.Parse 成功、Host 非空、userinfo 形态合法
// （只允许「无 userinfo」「user」「user:pass」三种；拒绝 `:pass@` 这种空用户名配
// 非空密码的形态——它在 URL 语法上合法但几乎总是配置笔误）。
func ParseProxyURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("解析失败: %w", err)
	}
	scheme := strings.ToLower(u.Scheme)
	if !proxySchemes[scheme] {
		return nil, fmt.Errorf("不支持的 scheme %q（仅支持 http/https/socks5/socks5h）", u.Scheme)
	}
	u.Scheme = scheme // 归一大小写，下游按小写判定
	if u.Host == "" {
		return nil, fmt.Errorf("缺少 host（期望形如 %s://host:port）", scheme)
	}
	if u.User != nil && u.User.Username() == "" {
		return nil, fmt.Errorf("userinfo 形态非法：用户名不能为空（只支持 user 或 user:pass）")
	}
	// 代理 URL 不应带路径/查询/片段：带上了多半是粘贴时混入了别的东西，
	// 而标准库会静默忽略它们，导致「配了但没按预期生效」的困惑。
	if u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("代理 URL 不应带路径/查询/片段（只接受 scheme://[user:pass@]host:port）")
	}
	return u, nil
}

// applyProxy 把已校验的代理 URL 接到 Transport 上。tr 必须是 newTransport()
// 的产物（复用其连接层加固）。
//
// 两条接线路径互斥：
//   - HTTP/HTTPS 代理：标准库的 Proxy 回调负责建隧道，DialContext 保持直连语义
//     （它连的是代理服务器本身，故我们的 newDialer 加固继续生效）。
//   - SOCKS5：标准库的 Proxy 机制不认 SOCKS5，必须换成自定义 DialContext；
//     同时把 Proxy 置 nil，否则标准库会再尝试一次 HTTP 代理语义（SOCKS5 URL 会让
//     它报 unsupported protocol scheme）。
func applyProxy(tr *http.Transport, u *url.URL) error {
	switch u.Scheme {
	case "http", "https":
		tr.Proxy = http.ProxyURL(u)
		return nil
	case "socks5", "socks5h":
		var auth *proxy.Auth
		if u.User != nil {
			pass, _ := u.User.Password()
			auth = &proxy.Auth{User: u.User.Username(), Password: pass}
		}
		// 第四个参数 forward 传我们的 newDialer()（而非 nil / proxy.Direct）：
		// SOCKS5 握手要先连代理服务器本身，这个「连代理」的 TCP 层必须保留
		// dialTimeout=10s / dialKeepAlive=15s 加固。proxy.FromURL 内部用
		// proxy.Direct（无超时参数）作 base，会丢加固——故不走 FromURL。
		d, err := proxy.SOCKS5("tcp", u.Host, auth, newDialer())
		if err != nil {
			return fmt.Errorf("构造 SOCKS5 dialer 失败: %w", err)
		}
		cd, ok := d.(proxy.ContextDialer)
		if !ok {
			return fmt.Errorf("SOCKS5 dialer 未实现 ContextDialer（x/net 版本不兼容？）")
		}
		tr.DialContext = cd.DialContext
		tr.Proxy = nil
		return nil
	}
	return fmt.Errorf("不支持的 scheme %q", u.Scheme)
}

// ProxyTransportFor 构造带指定代理的出站 Transport，供 login CLI 与网关共用
// （SOCKS5 接线只此一份，避免两处漂移）。
//
// raw 为空 → (nil, nil)：调用方自行决定回落（login 用直连、网关用环境变量代理）。
// raw 非法 → ("", err)：调用方决定是 WARN 回落还是直接报错（login 属交互式工具，
// 建议直接报错让用户立刻看到；网关走 normalize 的 WARN 回落口径）。
func ProxyTransportFor(raw string) (*http.Transport, error) {
	u, err := ParseProxyURL(raw)
	if err != nil {
		return nil, err
	}
	if u == nil {
		return nil, nil
	}
	tr := newTransport()
	if err := applyProxy(tr, u); err != nil {
		return nil, err
	}
	return tr, nil
}

// ProxyDesc 返回代理 URL 的脱敏描述（scheme + host，不含用户名密码），供 /status
// 透出与日志使用。u 为 nil 时返回空串。
//
// 密码绝不能进 /status：那是运维端点，可能被截图、被采集、被日志记录。
// scheme + host:port 已足够运维核对「是不是我设的那个代理」。
func ProxyDesc(u *url.URL) string {
	if u == nil {
		return ""
	}
	return u.Scheme + "://" + u.Host
}
