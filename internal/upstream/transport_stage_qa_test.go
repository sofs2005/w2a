// transport_stage_qa_test.go QA 独立验证（A 段对抗表）：工程师的判定表覆盖了主形态，
// 这里补的是「包装层数/优先级/边界」——分类器的次序一旦写反（例如把 OpError 判在
// DNSError 之前），线上那 4/4 次 DNS 超时就会重新变成 Dial，进而变成「换号」而不是
// 「同号重试」。故逐条钉住优先级与包装深度。
package upstream

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"syscall"
	"testing"
)

// TestQATransportStagePrecedence 优先级与包装深度：DNSError 必须穿透任意层
// OpError 优先于拨号判定；DeadlineExceeded 归 header 阶段（见报告 Minor）。
func TestQATransportStagePrecedence(t *testing.T) {
	dns := &net.DNSError{Err: "i/o timeout", Name: "www.workbuddy.ai", Server: "127.0.0.11:53", IsTimeout: true}
	cases := []struct {
		name string
		err  error
		want TransportStage
	}{
		// —— 优先级：DNSError 胜出（哪怕被 dial/read/proxyconnect 层层包住）——
		{"OpError{read}→DNSError（解析在写读里）", &net.OpError{Op: "read", Err: dns}, StageDNS},
		{"OpError{dial}→OpError{dial}→DNSError（双层）", &net.OpError{Op: "dial", Err: &net.OpError{Op: "dial", Err: dns}}, StageDNS},
		{"fmt.Errorf 包装的线上形态", fmt.Errorf("call failed: %w",
			&url.Error{Op: "Post", URL: "https://x", Err: &net.OpError{Op: "dial", Err: dns}}), StageDNS},
		{"url.Error 直接包 DNSError", &url.Error{Op: "Post", URL: "https://x", Err: dns}, StageDNS},

		// —— 拨号：无 DNSError 时的 Op 标记 ——
		{"OpError{dial}→ECONNREFUSED（双层 dial）",
			&net.OpError{Op: "dial", Err: &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}}, StageDial},
		{"OpError{dial}→context.DeadlineExceeded（拨号超时）",
			&net.OpError{Op: "dial", Err: context.DeadlineExceeded}, StageDial},
		{"OpError{dial}→EHOSTUNREACH", &net.OpError{Op: "dial", Err: syscall.EHOSTUNREACH}, StageDial},
		{"proxyconnect（线路）", &net.OpError{Op: "proxyconnect", Net: "tcp", Err: syscall.ECONNREFUSED}, StageDial},

		// —— TLS ——
		{"url.Error→RecordHeaderError", &url.Error{Op: "Post", URL: "https://x",
			Err: tls.RecordHeaderError{Msg: "first record does not look like a TLS handshake"}}, StageTLS},
		{"x509.HostnameError", x509.HostnameError{Certificate: nil, Host: "x"}, StageTLS},
		{"x509.CertificateInvalidError", x509.CertificateInvalidError{Reason: x509.Expired}, StageTLS},
		{"url.Error→TLS handshake timeout 文案",
			&url.Error{Op: "Post", URL: "https://x", Err: errors.New("net/http: TLS handshake timeout")}, StageTLS},

		// —— header 阶段 ——
		{"url.Error→timeout awaiting response headers",
			&url.Error{Op: "Post", URL: "https://x", Err: errors.New("net/http: timeout awaiting response headers")}, StageHeader},
		{"url.Error→context.DeadlineExceeded（客户端/自设 deadline）",
			&url.Error{Op: "Post", URL: "https://x", Err: context.DeadlineExceeded}, StageHeader},
		{"os.ErrDeadlineExceeded", os.ErrDeadlineExceeded, StageHeader},
		{"OpError{read}→os.ErrDeadlineExceeded（读响应超时）",
			&net.OpError{Op: "read", Err: os.ErrDeadlineExceeded}, StageHeader},

		// —— other（不得误进 header，否则会白罚账号）——
		{"url.Error→context.Canceled", &url.Error{Op: "Post", URL: "https://x", Err: context.Canceled}, StageOther},
		{"url.Error→EOF（连接被关）", &url.Error{Op: "Post", URL: "https://x", Err: io.EOF}, StageOther},
		{"OpError{write}→EPIPE", &net.OpError{Op: "write", Err: syscall.EPIPE}, StageOther},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := TransportStageOf(c.err); got != c.want {
				t.Fatalf("TransportStageOf(%v) = %s, want %s", c.err, got, c.want)
			}
		})
	}
}

// TestQAConnectStageExcludesHeaderAndOther 连接期分组的边界：只有 DNS/Dial/TLS 属于
// 「请求字节还没发出去」——handler 的同号重试只认这一组，多收一个就会把 header 超时
// 也拿去重试（白等 MaxRetry 次 header_timeout）。
func TestQAConnectStageExcludesHeaderAndOther(t *testing.T) {
	for _, s := range []TransportStage{StageDNS, StageDial, StageTLS} {
		if !s.IsConnectStage() {
			t.Errorf("%s 应属于连接期", s)
		}
	}
	for _, s := range []TransportStage{StageNone, StageHeader, StageOther} {
		if s.IsConnectStage() {
			t.Errorf("%s 不应属于连接期（否则 header 超时会被重试）", s)
		}
	}
}

// TestQATransportStageString 日志里的 stage= 短名（排障第一现场，改了会让既有日志
// 检索式失效）。
func TestQATransportStageString(t *testing.T) {
	want := map[TransportStage]string{
		StageNone: "none", StageDNS: "dns", StageDial: "dial",
		StageTLS: "tls", StageHeader: "header", StageOther: "other",
	}
	for s, w := range want {
		if got := s.String(); got != w {
			t.Errorf("%d.String() = %q want %q", s, got, w)
		}
	}
}
