package upstream

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/andybalholm/brotli"

	"workbuddy2api/internal/auth"
)

// gzipBytes / deflateBytes 等构造各种编码的响应体，供 decompressBody 回环测试。
func gzipBytes(t *testing.T, s string) []byte {
	t.Helper()
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	if _, err := w.Write([]byte(s)); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return b.Bytes()
}

func zlibDeflateBytes(t *testing.T, s string) []byte {
	t.Helper()
	var b bytes.Buffer
	w := zlib.NewWriter(&b)
	if _, err := w.Write([]byte(s)); err != nil {
		t.Fatalf("zlib write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("zlib close: %v", err)
	}
	return b.Bytes()
}

func rawDeflateBytes(t *testing.T, s string) []byte {
	t.Helper()
	var b bytes.Buffer
	w, err := flate.NewWriter(&b, flate.DefaultCompression)
	if err != nil {
		t.Fatalf("flate writer: %v", err)
	}
	if _, err := w.Write([]byte(s)); err != nil {
		t.Fatalf("flate write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("flate close: %v", err)
	}
	return b.Bytes()
}

func brotliBytes(t *testing.T, s string) []byte {
	t.Helper()
	var b bytes.Buffer
	w := brotli.NewWriter(&b)
	if _, err := w.Write([]byte(s)); err != nil {
		t.Fatalf("brotli write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("brotli close: %v", err)
	}
	return b.Bytes()
}

// TestDecompressBody 回环：官方 axios 会接受的那几种编码，我们必须都能解回原文。
// 这是「显式设置 Accept-Encoding 后 Transport 不再透明解压」的必要配套。
func TestDecompressBody(t *testing.T) {
	const want = "data: {\"choices\":[{\"delta\":{\"content\":\"你好\"}}]}\n\n"

	cases := []struct {
		name string
		enc  string
		body []byte
	}{
		{"gzip", "gzip", gzipBytes(t, want)},
		{"x-gzip", "x-gzip", gzipBytes(t, want)},
		{"compress", "compress", gzipBytes(t, want)},
		{"deflate-zlib", "deflate", zlibDeflateBytes(t, want)},
		{"deflate-raw", "deflate", rawDeflateBytes(t, want)},
		{"br", "br", brotliBytes(t, want)},
		{"identity", "identity", []byte(want)},
		{"empty", "", []byte(want)},
		{"upper-gzip", "GZIP", gzipBytes(t, want)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Encoding": []string{tc.enc}},
				Body:       io.NopCloser(bytes.NewReader(tc.body)),
			}
			rc, err := decompressBody(resp)
			if err != nil {
				t.Fatalf("decompressBody(%q): %v", tc.enc, err)
			}
			defer rc.Close()
			got, err := io.ReadAll(rc)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if string(got) != want {
				t.Errorf("got %q want %q", got, want)
			}
		})
	}
}

// TestDecompressBodyZstdUnsupported 我们从不 advertise zstd；上游若真返回，
// 必须是显式错误而不是把压缩字节当明文交上去。
func TestDecompressBodyZstdUnsupported(t *testing.T) {
	resp := &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Encoding": []string{"zstd"}},
		Body:       io.NopCloser(strings.NewReader("whatever")),
	}
	if _, err := decompressBody(resp); err == nil {
		t.Fatal("want error for zstd, got nil")
	}
}

// TestChatStreamDecompressesResponse 端到端：上游按我们声明的 Accept-Encoding 返回
// gzip 压缩的 SSE，网关拿到的 rc 必须是明文 SSE（否则 handler 解析全废）。
func TestChatStreamDecompressesResponse(t *testing.T) {
	const sse = "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"
	c := testClient(func(r *http.Request) (*http.Response, error) {
		// 出站必须只声明一次、且是我们对齐官方的取值。
		if got := r.Header.Get("Accept-Encoding"); got != chatAcceptEncoding {
			t.Errorf("outbound Accept-Encoding = %q want %q", got, chatAcceptEncoding)
		}
		return &http.Response{
			StatusCode: 200,
			Header: http.Header{
				"Content-Type":     []string{"text/event-stream"},
				"Content-Encoding": []string{"gzip"},
			},
			Body: io.NopCloser(bytes.NewReader(gzipBytes(t, sse))),
		}, nil
	})
	rc, status, _, err := c.ChatStreamContext(context.Background(),
		&auth.Auth{AccessToken: "at"}, []byte(`{"model":"m"}`), "", ChatMeta{ConversationRequestID: "req-dec"})
	if err != nil || status != 200 {
		t.Fatalf("status=%d err=%v", status, err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != sse {
		t.Errorf("stream not decompressed:\n got %q\nwant %q", got, sse)
	}
}

// TestChatStreamInvalidGzipIsTransportError 上游声称 gzip 但字节不是 gzip：
// 必须按传输层错误处理（换号），不能把压缩字节当 SSE 交给上层解析。
func TestChatStreamInvalidGzipIsTransportError(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 200,
			Header: http.Header{
				"Content-Type":     []string{"text/event-stream"},
				"Content-Encoding": []string{"gzip"},
			},
			Body: io.NopCloser(strings.NewReader("not-gzip-at-all")),
		}, nil
	})
	rc, _, _, err := c.ChatStreamContext(context.Background(),
		&auth.Auth{AccessToken: "at"}, []byte(`{"model":"m"}`), "", ChatMeta{ConversationRequestID: "req-bad-gzip"})
	if rc != nil {
		rc.Close()
	}
	if err == nil {
		t.Fatal("want transport error on invalid gzip, got nil")
	}
	if !strings.Contains(err.Error(), "decompress body") {
		t.Errorf("error should carry \"decompress body\", got: %v", err)
	}
}

// TestChatStreamErrorBodyDecompressed 错误分支同样要解压：上游 400 的信封也可能是
// gzip 的，不解就会把二进制丢进 Classify。
func TestChatStreamErrorBodyDecompressed(t *testing.T) {
	const body = `{"code":"11-128","msg":"request illegal"}`
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 400,
			Header: http.Header{
				"Content-Type":     []string{"application/json"},
				"Content-Encoding": []string{"gzip"},
			},
			Body: io.NopCloser(bytes.NewReader(gzipBytes(t, body))),
		}, nil
	})
	_, status, respBody, err := c.ChatStreamContext(context.Background(),
		&auth.Auth{AccessToken: "at"}, []byte(`{"model":"m"}`), "", ChatMeta{ConversationRequestID: "req-dec-err"})
	if status != 400 {
		t.Fatalf("status = %d want 400", status)
	}
	if err == nil {
		t.Fatal("want classified error, got nil")
	}
	if string(respBody) != body {
		t.Errorf("error body not decompressed: %q", respBody)
	}
}
