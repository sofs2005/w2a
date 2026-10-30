// transport_stage_test.go 钉住传输层错误「阶段」分类器的判定表（A 段）。
//
// 为什么值得单独测：这个分类器是「DNS/Dial/TLS 失败 ≠ 上游慢」这条修复的唯一判据。
// 判错的后果是线上实测过的那两条——报文把运维指向「上游慢」（真实原因是网关侧
// 解析），以及完全无关的账号被 NoteTimeout 冷却 60s。
package upstream

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/url"
	"syscall"
	"testing"
)

// netTimeoutErr 最朴素的 net.Error 超时（模拟 Transport.ResponseHeaderTimeout
// 之外的通用超时，如 http.Client.Timeout）。
type netTimeoutErr struct{}

func (netTimeoutErr) Error() string   { return "i/o timeout" }
func (netTimeoutErr) Timeout() bool   { return true }
func (netTimeoutErr) Temporary() bool { return true }

// TestTransportStageOf 判定表：每条都是线上/标准库真实错误形态的构造。
func TestTransportStageOf(t *testing.T) {
	// 线上实证的原始形态（容器内 DNS 超时，从日志里逐字抄下来的结构）：
	// Post "https://…": dial tcp: lookup www.workbuddy.ai on 127.0.0.11:53:
	// read udp 127.0.0.1:36144->127.0.0.11:53: i/o timeout
	dnsTimeout := &net.DNSError{
		Err:         "i/o timeout",
		Name:        "www.workbuddy.ai",
		Server:      "127.0.0.11:53",
		IsTimeout:   true,
		IsTemporary: true,
	}
	onlineDNSShape := &url.Error{
		Op:  "Post",
		URL: "https://www.workbuddy.ai/v2/chat/completions",
		Err: &net.OpError{Op: "dial", Net: "tcp", Err: dnsTimeout},
	}

	cases := []struct {
		name string
		err  error
		want TransportStage
	}{
		{"nil", nil, StageNone},
		{"DNS 超时（裸）", dnsTimeout, StageDNS},
		{"DNS 不存在（非超时也算 DNS）", &net.DNSError{Err: "no such host", Name: "x", IsNotFound: true}, StageDNS},
		{"线上形态 url.Error→OpError{dial}→DNSError", onlineDNSShape, StageDNS},
		{"拨号被拒 OpError{dial}", &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}, StageDial},
		{"拨号被拒（url.Error 包装）", &url.Error{Op: "Post", URL: "https://x", Err: &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}}, StageDial},
		// 账号绑了出口线路（HTTP 代理）时，net/http 用 Op="proxyconnect" 包住内层
		// 拨号错误——实测形态 `proxyconnect tcp: dial tcp 1.2.3.4:1080: connect:
		// connection refused`，必须归 StageDial（否则「线路挂了」会被当成未知抖动
		// → NoteFailures + 换号，账号被线路连坐）。
		{"代理拨号被拒 OpError{proxyconnect}", &url.Error{Op: "Post", URL: "https://x", Err: &net.OpError{
			Op: "proxyconnect", Net: "tcp",
			Err: &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED},
		}}, StageDial},
		{"代理主机解析失败（proxyconnect→DNSError）", &url.Error{Op: "Post", URL: "https://x", Err: &net.OpError{
			Op: "proxyconnect", Net: "tcp",
			Err: &net.OpError{Op: "dial", Net: "tcp", Err: &net.DNSError{Err: "no such host", Name: "proxy.corp", IsNotFound: true}},
		}}, StageDNS},
		{"读写中断 OpError{read}", &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}, StageOther},
		{"TLS RecordHeaderError", tls.RecordHeaderError{Msg: "first record does not look like a TLS handshake"}, StageTLS},
		{"TLS 握手文案（不可导出 httpError）", errors.New("net/http: TLS handshake timeout"), StageTLS},
		{"TLS 证书文案", &url.Error{Op: "Post", URL: "https://x", Err: errors.New("tls: failed to verify certificate: x509: certificate signed by unknown authority")}, StageTLS},
		{"x509.UnknownAuthorityError 类型", x509.UnknownAuthorityError{}, StageTLS},
		{"header 阶段超时文案", errors.New("net/http: timeout awaiting response headers"), StageHeader},
		{"通用 net.Error 超时", netTimeoutErr{}, StageHeader},
		{"context.DeadlineExceeded", context.DeadlineExceeded, StageHeader},
		{"客户端断连 context.Canceled", context.Canceled, StageOther},
		{"普通错误", errors.New("boom"), StageOther},
		{"业务错误（已是 *Error 信封）", &Error{Kind: ErrRouteUnavailable}, StageOther},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := TransportStageOf(c.err); got != c.want {
				t.Fatalf("TransportStageOf(%v) = %v(%d), want %v(%d)", c.err, got, got, c.want, c.want)
			}
		})
	}
}

// TestTransportStageConnectGroup 连接建立期（请求字节还没发出去）的分组语义：
// DNS/Dial/TLS 属于它，Header/Other/None 不属于——handler 的同号重试只认这一组。
func TestTransportStageConnectGroup(t *testing.T) {
	for _, c := range []struct {
		stage TransportStage
		want  bool
	}{
		{StageNone, false},
		{StageDNS, true},
		{StageDial, true},
		{StageTLS, true},
		{StageHeader, false},
		{StageOther, false},
	} {
		if got := c.stage.IsConnectStage(); got != c.want {
			t.Errorf("%v.IsConnectStage() = %v, want %v", c.stage, got, c.want)
		}
	}
}

// TestTransportStageString 日志用短名（stage=dns 这种形态）钉住，避免日志口径漂移。
func TestTransportStageString(t *testing.T) {
	want := map[TransportStage]string{
		StageNone:   "none",
		StageDNS:    "dns",
		StageDial:   "dial",
		StageTLS:    "tls",
		StageHeader: "header",
		StageOther:  "other",
	}
	for stage, s := range want {
		if got := stage.String(); got != s {
			t.Errorf("TransportStage(%d).String() = %q, want %q", stage, got, s)
		}
	}
}
