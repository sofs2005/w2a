// decompress.go 出站显式声明 Accept-Encoding 后的响应侧解码。
//
// 为什么必须有这一层：Go 的 net/http.Transport 只在**它自己**添加 Accept-Encoding
// 时才插入透明解压（transport.go 的 DisableCompression=false 分支）；一旦请求上已经
// 有该头，Transport 视为「调用方自己管」，原样把压缩字节交回来。
//
// 我们的 chat 出站现在显式对齐官方 axios 的取值（见 headers.go chatAcceptEncoding），
// 所以响应必须自己解。其余出站（billing/refresh/models）没有显式设置该头，仍由
// Transport 透明解压，不受影响。
package upstream

import (
	"bufio"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/andybalholm/brotli"
)

// multiCloser 把解码器与底流绑成一个 ReadCloser：Close 先收解码器再关底流
// （解码器若持有缓冲，先放它才不会在已关闭的流上收尾）。
type multiCloser struct {
	io.Reader
	closers []io.Closer
}

func (m *multiCloser) Close() error {
	var first error
	for _, c := range m.closers {
		if c == nil {
			continue
		}
		if err := c.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// decompressBody 按响应头 Content-Encoding 包一层解码器。
//
// 取值与官方 axios 的 node adapter 解压分支逐条对应（同一份 CLI 包内
// `_request` 的 `switch (content-encoding)`）：
//
//	gzip / x-gzip / compress / x-compress → gzip（axios 用 zlib.createUnzip，
//	                                        它按魔数自识别，这几档实际都落到 gzip）
//	deflate                              → zlib，失败回落 raw deflate（axios 叠了
//	                                        createInflate + createUnzip 两层，等价于
//	                                        两者都试）
//	br                                   → brotli
//	zstd                                 → 不解（我们的 Accept-Encoding 从不带 zstd，
//	                                        上游不应返回；真返回则视为协议异常）
//
// 空 / identity 原样返回。未知编码原样返回（多给一次机会，胜过硬失败把可用响应打掉）。
//
// 返回的 ReadCloser 负责关闭底流，调用方只需关它一个。
func decompressBody(resp *http.Response) (io.ReadCloser, error) {
	if resp == nil || resp.Body == nil {
		return nil, fmt.Errorf("decompress: nil response body")
	}
	enc := ""
	if resp.Header != nil {
		enc = strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding")))
	}
	// 上游可能返回逗号分隔的多段编码（如 "gzip, br"），按顺序从外到内解。
	if enc == "" || enc == "identity" {
		return resp.Body, nil
	}
	var cur io.ReadCloser = resp.Body
	parts := strings.Split(enc, ",")
	for i := len(parts) - 1; i >= 0; i-- {
		part := strings.TrimSpace(parts[i])
		next, err := wrapDecoder(part, cur)
		if err != nil {
			cur.Close()
			return nil, err
		}
		cur = next
	}
	return cur, nil
}

// wrapDecoder 为单个 Content-Encoding 取值构造解码层。
func wrapDecoder(enc string, rc io.ReadCloser) (io.ReadCloser, error) {
	switch enc {
	case "", "identity":
		return rc, nil
	case "gzip", "x-gzip", "compress", "x-compress":
		gz, err := gzip.NewReader(rc)
		if err != nil {
			return nil, fmt.Errorf("decompress gzip: %w", err)
		}
		return &multiCloser{Reader: gz, closers: []io.Closer{gz, rc}}, nil
	case "deflate":
		return newDeflateReader(rc)
	case "br":
		// brotli.Reader 无状态、无 Close；只需把底流的 Close 透传出去。
		return &multiCloser{Reader: brotli.NewReader(rc), closers: []io.Closer{rc}}, nil
	case "zstd":
		return nil, fmt.Errorf("decompress: zstd not supported (we never advertise it)")
	default:
		// 未知编码：不冒险解，原样交回（由上层按明文处理）。
		return rc, nil
	}
}

// newDeflateReader 处理 `Content-Encoding: deflate` 的现实两义性：
// 标准要求 zlib 包装，但大量服务器发的是裸 deflate 流。官方 axios 同时挂了
// createInflate 与 createUnzip 两层，效果就是「两种都试」，这里用嗅探 2 字节头实现。
func newDeflateReader(rc io.ReadCloser) (io.ReadCloser, error) {
	br := bufio.NewReader(rc)
	head, _ := br.Peek(2)
	if len(head) == 2 && isZlibHeader(head[0], head[1]) {
		zr, err := zlib.NewReader(br)
		if err != nil {
			return nil, fmt.Errorf("decompress deflate(zlib): %w", err)
		}
		return &multiCloser{Reader: zr, closers: []io.Closer{zr, rc}}, nil
	}
	fr := flate.NewReader(br)
	return &multiCloser{Reader: fr, closers: []io.Closer{fr, rc}}, nil
}

// isZlibHeader 判断两字节是否构成 zlib 头（CM=8 且 FCHECK 校验通过）。
// 这是 RFC 1950 的标准判定，与 axios 依赖 zlib 自识别同义。
func isZlibHeader(b0, b1 byte) bool {
	if b0&0x0f != 8 {
		return false // CM 必须是 8（deflate）
	}
	if b0>>4 > 7 {
		return false // CINFO 上限 7
	}
	return (uint16(b0)<<8|uint16(b1))%31 == 0
}
