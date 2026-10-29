package upstream

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
)

// 渠道校验封套（与 fallback_model_missing_test.go 的 channelBody 同义，这里用
// 独立常量避免依赖另一文件的字面量，测试之间可单独阅读）。
const gzipChannelBody = `{"code":"11-128","msg":"request illegal, channel verification failed"}`

// sseOKBody 一个最小合法 SSE 流（成功分支只要求能读）。
const sseOKBody = "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"

// bigChatBody 构造一个 >64KB 的 chat 请求体（可压缩文本，长上下文形态）。
func bigChatBody() []byte {
	return []byte(`{"model":"m","messages":[{"role":"user","content":"` +
		strings.Repeat("上下文", 30000) + `"}]}`)
}

// TestOutboundBodyThreshold 小于阈值不压、超过阈值压且能解回原文。
func TestOutboundBodyThreshold(t *testing.T) {
	c := &Client{GzipBody: true}
	small := bytes.Repeat([]byte("a"), gzipBodyThreshold-1)
	if out, enc := c.outboundBody(small); enc != "" || !bytes.Equal(out, small) {
		t.Fatalf("小于阈值不应压缩（enc=%q len=%d）", enc, len(out))
	}
	big := bigChatBody()
	out, enc := c.outboundBody(big)
	if enc != "gzip" {
		t.Fatalf("超过阈值应压缩，got enc=%q", enc)
	}
	if len(out) >= len(big) {
		t.Fatalf("压缩后应更小：%d >= %d", len(out), len(big))
	}
	zr, err := gzip.NewReader(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	back, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(back, big) {
		t.Fatal("解压后与原文不一致")
	}
}

// TestOutboundBodyDisabled 关闭开关时永不压缩（缺省值即关闭，排障路径）。
func TestOutboundBodyDisabled(t *testing.T) {
	c := &Client{GzipBody: false}
	if _, enc := c.outboundBody(bigChatBody()); enc != "" {
		t.Fatalf("关闭后不应压缩，got %q", enc)
	}
	// New() 的默认值也必须是关闭（opt-in）：改默认值等于给全部流量打开一个
	// 已知的 11-128 触发源，这条断言是那个决定的锚点。
	if New().GzipBody {
		t.Fatal("New() 默认不应开启 gzip_body（opt-in，缺省 false）")
	}
}

// TestGzipUnsupported 只认 415 与「解析失败类」400。
func TestGzipUnsupported(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   bool
	}{
		{http.StatusUnsupportedMediaType, "", true},
		{http.StatusBadRequest, `{"msg":"Unmarshal chat params failed"}`, true},
		{http.StatusBadRequest, `{"msg":"invalid character '\x1f' looking for beginning of value"}`, true},
		{http.StatusBadRequest, `{"msg":"content-encoding not supported"}`, true},
		{http.StatusBadRequest, `{"msg":"model is required"}`, false},
		{http.StatusTooManyRequests, `{"code":6004}`, false},
		{http.StatusInternalServerError, `oops`, false},
	}
	for _, c := range cases {
		if got := gzipUnsupported(c.status, c.body); got != c.want {
			t.Errorf("gzipUnsupported(%d, %q) = %v, want %v", c.status, c.body, got, c.want)
		}
	}
}

// TestGzipChannelCheckRetriesUncompressed 压缩请求体命中渠道校验时，先去压缩重试
// 同一条路径（压缩本身就是「不像官方客户端」的信号：上游实测长上下文 gzip 后
// 稳定吃 11-128，同一 body 不压缩即 200）。
func TestGzipChannelCheckRetriesUncompressed(t *testing.T) {
	var encodings []string
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		encodings = append(encodings, r.Header.Get("Content-Encoding"))
		paths = append(paths, r.URL.Path)
		if r.Header.Get("Content-Encoding") == "gzip" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(gzipChannelBody))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(sseOKBody))
	}))
	defer srv.Close()

	c := New()
	c.GzipBody = true
	c.ChatBaseCN = srv.URL
	acct := &auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}
	body, status, raw, err := c.ChatStream(acct, bigChatBody(), "", ChatMeta{})
	if err != nil {
		t.Fatalf("status=%d err=%v body=%s", status, err, raw)
	}
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, raw)
	}
	if body != nil {
		body.Close()
	}
	if len(encodings) != 2 || encodings[0] != "gzip" || encodings[1] != "" {
		t.Fatalf("应先发压缩、再去压缩重试，实际 Content-Encoding 序列=%v", encodings)
	}
	// 去压缩重试必须发生在**同一条路径**上（先别急着换 /console）。
	if len(paths) != 2 || paths[0] != chatCompletionsPath || paths[1] != chatCompletionsPath {
		t.Fatalf("应在同一条路径上去压缩重试，实际路径=%v", paths)
	}
}

// TestGzipUnsupportedRetriesSamePath 上游明确拒收压缩体（415）时改未压缩重发
// 同一条路径，不消耗路径名额。
func TestGzipUnsupportedRetriesSamePath(t *testing.T) {
	var encodings []string
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		encodings = append(encodings, r.Header.Get("Content-Encoding"))
		paths = append(paths, r.URL.Path)
		if r.Header.Get("Content-Encoding") == "gzip" {
			w.WriteHeader(http.StatusUnsupportedMediaType)
			_, _ = w.Write([]byte(`{"msg":"unsupported media type"}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(sseOKBody))
	}))
	defer srv.Close()

	c := New()
	c.GzipBody = true
	c.ChatBaseCN = srv.URL
	acct := &auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}
	body, status, raw, err := c.ChatStream(acct, bigChatBody(), "", ChatMeta{})
	if err != nil {
		t.Fatalf("status=%d err=%v body=%s", status, err, raw)
	}
	if body != nil {
		body.Close()
	}
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, raw)
	}
	if len(encodings) != 2 || encodings[0] != "gzip" || encodings[1] != "" {
		t.Fatalf("Content-Encoding 序列 = %v, want [gzip \"\"]", encodings)
	}
	if len(paths) != 2 || paths[0] != chatCompletionsPath || paths[1] != chatCompletionsPath {
		t.Fatalf("未压缩重试应走同一条路径，实际路径=%v", paths)
	}
}

// TestGzipDisabledKeepsSingleUncompressedRequest 关闭开关（缺省）时，大请求体也
// 只发一次未压缩请求：路径序列与 Content-Encoding 都是改造前的形态（零回归）。
func TestGzipDisabledKeepsSingleUncompressedRequest(t *testing.T) {
	var encodings []string
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		encodings = append(encodings, r.Header.Get("Content-Encoding"))
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(sseOKBody))
	}))
	defer srv.Close()

	c := New() // GzipBody 默认 false
	c.ChatBaseCN = srv.URL
	acct := &auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}
	body, status, raw, err := c.ChatStream(acct, bigChatBody(), "", ChatMeta{})
	if err != nil {
		t.Fatalf("status=%d err=%v body=%s", status, err, raw)
	}
	if body != nil {
		body.Close()
	}
	if len(encodings) != 1 || encodings[0] != "" {
		t.Fatalf("关闭时不应带 Content-Encoding，实际序列=%v", encodings)
	}
	if len(paths) != 1 || paths[0] != chatCompletionsPath {
		t.Fatalf("关闭时路径序列应原样（/v2 一次），实际=%v", paths)
	}
}
