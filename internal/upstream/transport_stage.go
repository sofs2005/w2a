// transport_stage.go 传输层失败的「阶段」分类器（单一事实来源）。
//
// 为什么需要它（线上实证，本次修复的起点）：上一轮把「超时」从「网络抖动」里拆出来
// 之后，线上仍持续看到 code=upstream_timeout，而日志里 4/4 次全都不是上游慢，而是
// **容器内 DNS 解析超时**：
//
//	WARN: [server] upstream timeout acct=foginsky(0aca161f) model="deepseek-v4.1-flash" i=0
//	  sticky=true cooled=true err=Post "https://www.workbuddy.ai/v2/chat/completions":
//	  dial tcp: lookup www.workbuddy.ai on 127.0.0.11:53: read udp 127.0.0.1:36144->127.0.0.11:53: i/o timeout
//
// 根因是判据太粗：`net.Error.Timeout()` 一刀切，而 **DNS/Dial/TLS 阶段**的失败
// （请求字节还没发出去）与 **等响应头**阶段的超时（请求已发出、上游首字节慢）在
// 语义上完全不同：
//   - 前者与账号无关、与上游模型无关（同一个域名、同一个解析器、同一条出口），
//     报文说 "another account would hit the same slow upstream" 会把人往上游方向带；
//     把它喂进 NoteTimeout 还会**误冷却账号**（foginsky 被摘出池 60s，与它毫无关系）。
//   - 后者才是「上游慢」，止损设计（不换号、达阈冷却 + 解绑粘性）针对的正是它。
//
// 本文件只做**分类**，不做处置：处置在 handler（连接阶段 → 同号有界重试 +
// gateway_network_error；header 阶段 → 超时止损；其余 → 换号）。
package upstream

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"strings"
)

// TransportStage 传输层失败的阶段——即请求走到哪一步挂的。
//
// 语义分组（handler 的处置据此分流，见 handler.chatCompletions 的传输层错误分支）：
//   - StageDNS / StageDial / StageTLS：**请求字节还没发出去**。与账号无关，换号也
//     解析同一个域名、拨同一个地址（例外：账号绑了出口线路，那是该线路的问题，
//     见 handler 的 route 记账）。
//   - StageHeader：请求已发出，等响应头（首字节）超时。这才是「上游慢」。
//   - StageOther：其余传输层抖动（连接被重置、读写中断、客户端断连等）。
type TransportStage int

const (
	// StageNone 不是传输层失败（err 为 nil，或已分类的业务错误）。
	StageNone TransportStage = iota
	// StageDNS 域名解析失败/超时（*net.DNSError，含 IsTimeout/IsTemporary 全部形态）。
	StageDNS
	// StageDial TCP 拨号失败（*net.OpError 且 Op=="dial"，如 connection refused）。
	StageDial
	// StageTLS TLS 握手/证书校验失败。
	StageTLS
	// StageHeader 请求已发出，等响应头超时（net/http 的
	// "timeout awaiting response headers"，或任何非上述阶段的 net.Error 超时）。
	StageHeader
	// StageOther 其余传输层抖动（读写中断、连接重置、客户端断连……）。
	StageOther
)

// String 日志/排障用的短名（stage=dns 这种形态，比数字可读）。
func (s TransportStage) String() string {
	switch s {
	case StageNone:
		return "none"
	case StageDNS:
		return "dns"
	case StageDial:
		return "dial"
	case StageTLS:
		return "tls"
	case StageHeader:
		return "header"
	case StageOther:
		return "other"
	default:
		return "unknown"
	}
}

// IsConnectStage 报告该阶段是否属于「连接建立期」（DNS / TCP / TLS）——即请求字节
// 还没发出去。handler 用它决定走「同号有界重试」而不是「超时止损 / 换号」。
func (s TransportStage) IsConnectStage() bool {
	return s == StageDNS || s == StageDial || s == StageTLS
}

// TransportStageOf 判定一个传输层错误的失败阶段（errors.As 逐层剥离包装：
// *url.Error → *net.OpError → *net.DNSError / syscall.Errno / tls/x509 错误）。
//
// 判定次序（先具体后笼统，顺序即优先级）：
//  1. *net.DNSError → StageDNS。**必须排在 *net.OpError 之前**：DNS 失败在标准库
//     里是 `*net.OpError{Op:"dial", Err:*net.DNSError}`（见线上实证的
//     "dial tcp: lookup …: i/o timeout"），先判 DNSError 才能把它从「拨号」里拆出来。
//     IsTimeout/IsTemporary 一律不区分——对网关而言「解析不了」就是解析不了。
//  2. *net.OpError 且 Op=="dial"（或 Op=="proxyconnect"，即连代理服务器的拨号）
//     → StageDial（connection refused / 无路由 / 拨号超时 / 代理不可达）。
//  3. TLS：tls.RecordHeaderError、tls.CertificateVerificationError、x509.* 家族，
//     以及 "tls: " / "x509: " 前缀与 net/http 的 "TLS handshake timeout"
//     （后者是 http 包内部不可导出的 httpError，只能按文案认）。
//  4. net/http 的 "timeout awaiting response headers"（同样是不可导出的 httpError），
//     以及任何**剩下的** net.Error 超时 → StageHeader。
//  5. 其余 → StageOther。
func TransportStageOf(err error) TransportStage {
	if err == nil {
		return StageNone
	}
	// 1) DNS 解析（含 OpError{dial, DNSError} 形态，故先于拨号判定）。
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return StageDNS
	}
	// 2) TCP 拨号：*net.OpError.Op 就是阶段标记（"dial"/"read"/"write"）。
	//    "proxyconnect" 也归此：那是「连代理服务器本身」的拨号（net/http 在走
	//    HTTP 代理时用 Op="proxyconnect" 包住内层的 dial 错误，实测形态
	//    `proxyconnect tcp: dial tcp 1.2.3.4:1080: connect: connection refused`）。
	//    它同样发生在请求字节发出去之前，语义与直连拨号完全一致——账号绑了出口
	//    线路时，这正是「线路挂了」最典型的形态（见 handler 的 route 记账）。
	var opErr *net.OpError
	if errors.As(err, &opErr) && (opErr.Op == "dial" || opErr.Op == "proxyconnect") {
		return StageDial
	}
	// 3) TLS 握手 / 证书。
	if isTLSTransportError(err) {
		return StageTLS
	}
	// 4) 等响应头超时：不可导出的 *http.httpError 只能按文案认；其余 net.Error 超时
	//    （ResponseHeaderTimeout、HTTP.Client.Timeout、context.DeadlineExceeded）同归
	//    此阶段——它们都发生在「请求已发出、响应未到」的窗口里。
	if strings.Contains(err.Error(), "timeout awaiting response headers") {
		return StageHeader
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return StageHeader
	}
	// 5) 其余传输层抖动（连接被重置、读写中断、客户端断连……）。
	return StageOther
}

// isTLSTransportError 报告该错误是否属于 TLS 握手/证书阶段。
//
// 类型判定优先（稳定），文案判定兜底（标准库把部分 TLS 失败包成不可导出的
// httpError，例如 "net/http: TLS handshake timeout"）。
func isTLSTransportError(err error) bool {
	var rhErr tls.RecordHeaderError
	if errors.As(err, &rhErr) {
		return true
	}
	var cvErr *tls.CertificateVerificationError
	if errors.As(err, &cvErr) {
		return true
	}
	var uaErr x509.UnknownAuthorityError
	if errors.As(err, &uaErr) {
		return true
	}
	var hnErr x509.HostnameError
	if errors.As(err, &hnErr) {
		return true
	}
	var ciErr x509.CertificateInvalidError
	if errors.As(err, &ciErr) {
		return true
	}
	var srErr x509.SystemRootsError
	if errors.As(err, &srErr) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "tls: ") ||
		strings.Contains(msg, "x509: ") ||
		strings.Contains(msg, "TLS handshake timeout")
}
