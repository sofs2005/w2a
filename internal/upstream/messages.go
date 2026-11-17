// messages.go Claude 模型在 CN 域改走 Anthropic Messages 路由。
//
// 背景（社区 2026-10-24 实测，CN 域 copilot.tencent.com）：
//   - /v1/messages 只服务 Claude 系模型（claude-opus-5.5 / claude-sonnet-5.5 /
//     claude-opus-5 / claude-opus-4.8-1m 均以 Anthropic 流式应答），其他模型 400/500；
//   - 收益是**档位真实生效**：chat 端点只认 reasoning_effort 且各档输出长度几乎相同
//     （同题四跑：low 中位 3884、max 5144 输出 token），Messages 的
//     output_config.effort 在 max 档实测中位 30850（同题同模型），是 chat 的 5 倍；
//   - WorkBuddy AI 域没有这条路由（/v1/messages 404），所以只在 CN 域启用。
//
// 对外仍是 OpenAI chat.completions：这里做 OpenAI 请求 → Anthropic 请求、
// Anthropic SSE → OpenAI SSE 的双向转换，客户端无感。
//
// 已知边界：不发送 thinking 块（OpenAI 客户端的 reasoning_content 不上行）。
// 社区实测「无签名的 thinking 块单独存在」可 200，但一旦旁边有带签名的块就 11159；
// 不发送就从根上没有这个问题。
package upstream

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/logfmt"
)

// messagesPath CN 域的 Anthropic Messages 路由。
const messagesPath = "/v1/messages"

// messagesNative 报告该账号+模型是否应走 Messages：CN 域（含云托管企业域）且模型是 Claude。
func messagesNative(a authLike, model string) bool {
	if a == nil || a.IsGlobal() {
		return false
	}
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), "claude-")
}

// authLike 让 messagesNative 的签名不直接依赖具体类型（便于测试）。
type authLike interface {
	IsGlobal() bool
}

// anthropicVersion 是 @ai-sdk/anthropic 固定发送的协议版本头。
const anthropicVersion = "2023-06-01"

// errMessagesNotApplicable 表示这条请求不适合走 Messages（body 无法解析、或转换后
// 没有可用的 user 消息）。调用方据此回落到 chat，而不是把转换失败当成上游错误。
var errMessagesNotApplicable = errors.New("messages: request not convertible")

// buildMessagesBody 把 OpenAI chat 请求体转成 Anthropic Messages 请求体。
// 无法解析时返回 errMessagesNotApplicable（调用方回落到 chat 路径，绝不把坏 body
// 发给另一条路由）。
func buildMessagesBody(openAI []byte) ([]byte, error) {
	var src map[string]any
	if err := json.Unmarshal(openAI, &src); err != nil {
		return nil, fmt.Errorf("%w: %v", errMessagesNotApplicable, err)
	}
	out := map[string]any{
		"model":  src["model"],
		"stream": true,
	}
	if mt, ok := src["max_tokens"]; ok {
		out["max_tokens"] = mt
	} else {
		out["max_tokens"] = 8192
	}
	var system []string
	var msgs []any
	if list, ok := src["messages"].([]any); ok {
		for _, raw := range list {
			m, _ := raw.(map[string]any)
			if m == nil {
				continue
			}
			role, _ := m["role"].(string)
			role = strings.ToLower(strings.TrimSpace(role))
			switch role {
			case "system", "developer":
				if s := contentText(m["content"]); s != "" {
					system = append(system, s)
				}
			case "user":
				msgs = append(msgs, map[string]any{
					"role":    "user",
					"content": anthropicContent(m["content"]),
				})
			case "tool":
				id, _ := m["tool_call_id"].(string)
				msgs = append(msgs, map[string]any{
					"role": "user",
					"content": []any{map[string]any{
						"type":        "tool_result",
						"tool_use_id": id,
						"content":     contentText(m["content"]),
					}},
				})
			case "assistant":
				msgs = append(msgs, anthropicAssistant(m))
			}
		}
	}
	if len(system) > 0 {
		out["system"] = strings.Join(system, "\n\n")
	}
	msgs = normalizeAnthropicMessages(msgs)
	if len(msgs) == 0 {
		return nil, errMessagesNotApplicable
	}
	out["messages"] = msgs
	if tools, ok := src["tools"].([]any); ok {
		out["tools"] = anthropicTools(tools)
	}
	if tc := anthropicToolChoice(src["tool_choice"]); tc != nil {
		out["tool_choice"] = tc
	}
	// 档位：Anthropic 侧用 output_config.effort，且需要 thinking 打开。
	if eff := effortOf(src); eff != "" {
		out["thinking"] = map[string]any{"type": "adaptive"}
		out["output_config"] = map[string]any{"effort": eff}
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	return raw, nil
}

// effortOf 取请求声明的档位（reasoning_effort / reasoning.effort）。
// none/minimal 不当作 Anthropic 档位（none = 明确不思考，omit）。
func effortOf(src map[string]any) string {
	eff := ""
	if s, ok := src["reasoning_effort"].(string); ok {
		eff = s
	} else if r, ok := src["reasoning"].(map[string]any); ok {
		if s, ok := r["effort"].(string); ok {
			eff = s
		}
	}
	switch strings.ToLower(strings.TrimSpace(eff)) {
	case "", "none", "minimal":
		return ""
	}
	return strings.ToLower(strings.TrimSpace(eff))
}

// normalizeAnthropicMessages 整形消息序列以符合 Anthropic 的硬约束：
//  1. 相邻同角色必须合并 —— OpenAI 的并行工具结果是一条条独立的 "tool" 消息，
//     转过来是连续多条 user；Anthropic 要求 user/assistant 交替。
//  2. 首条必须是 user；历史以 assistant 开场时丢弃开头的 assistant 段。
func normalizeAnthropicMessages(msgs []any) []any {
	merged := make([]any, 0, len(msgs))
	for _, raw := range msgs {
		m, _ := raw.(map[string]any)
		if m == nil {
			continue
		}
		if n := len(merged); n > 0 {
			prev := merged[n-1].(map[string]any)
			if prev["role"] == m["role"] {
				prev["content"] = mergeContent(prev["content"], m["content"])
				continue
			}
		}
		merged = append(merged, m)
	}
	for len(merged) > 0 && merged[0].(map[string]any)["role"] != "user" {
		merged = merged[1:]
	}
	return merged
}

// mergeContent 把两段 content（字符串或块数组）按序拼成一个块数组。
// tool_result 块自然保持在 user 文本之前（OpenAI 历史里工具结果本来就先于用户发言）。
func mergeContent(a, b any) []any {
	return append(contentBlocks(a), contentBlocks(b)...)
}

// contentBlocks 把字符串或块数组统一成块数组（空串不产生空文本块）。
func contentBlocks(v any) []any {
	switch c := v.(type) {
	case string:
		if c == "" {
			return nil
		}
		return []any{map[string]any{"type": "text", "text": c}}
	case []any:
		return c
	}
	return nil
}

// anthropicContent 把 OpenAI content 转成 Anthropic content（字符串或块数组）。
func anthropicContent(v any) any {
	switch c := v.(type) {
	case string:
		return c
	case []any:
		out := make([]any, 0, len(c))
		for _, raw := range c {
			p, _ := raw.(map[string]any)
			if p == nil {
				continue
			}
			switch p["type"] {
			case "text":
				if s, ok := p["text"].(string); ok {
					out = append(out, map[string]any{"type": "text", "text": s})
				}
			case "image_url":
				if img := anthropicImage(p["image_url"]); img != nil {
					out = append(out, img)
				}
			}
		}
		if len(out) == 0 {
			return ""
		}
		return out
	}
	return ""
}

// anthropicImage 把 OpenAI image_url 转成 Anthropic image 块（支持 data: base64；
// 远端 URL 走 source.type=url）。
func anthropicImage(v any) map[string]any {
	url := ""
	switch x := v.(type) {
	case string:
		url = x
	case map[string]any:
		if s, ok := x["url"].(string); ok {
			url = s
		}
	}
	if url == "" {
		return nil
	}
	if strings.HasPrefix(url, "data:") {
		// data:image/png;base64,xxxx
		comma := strings.Index(url, ",")
		if comma < 0 {
			return nil
		}
		head := url[:comma]
		media := strings.TrimPrefix(strings.SplitN(head, ";", 2)[0], "data:")
		return map[string]any{
			"type": "image",
			"source": map[string]any{
				"type":       "base64",
				"media_type": media,
				"data":       url[comma+1:],
			},
		}
	}
	return map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": url}}
}

// anthropicAssistant 转换一条 assistant 消息：文本 + tool_calls → content 块。
func anthropicAssistant(m map[string]any) map[string]any {
	var blocks []any
	if s := contentText(m["content"]); s != "" {
		blocks = append(blocks, map[string]any{"type": "text", "text": s})
	}
	if calls, ok := m["tool_calls"].([]any); ok {
		for _, raw := range calls {
			call, _ := raw.(map[string]any)
			if call == nil {
				continue
			}
			fn, _ := call["function"].(map[string]any)
			if fn == nil {
				continue
			}
			id, _ := call["id"].(string)
			name, _ := fn["name"].(string)
			input := map[string]any{}
			if args, ok := fn["arguments"].(string); ok && strings.TrimSpace(args) != "" {
				_ = json.Unmarshal([]byte(args), &input)
			}
			blocks = append(blocks, map[string]any{
				"type":  "tool_use",
				"id":    id,
				"name":  name,
				"input": input,
			})
		}
	}
	if len(blocks) == 0 {
		blocks = append(blocks, map[string]any{"type": "text", "text": ""})
	}
	return map[string]any{"role": "assistant", "content": blocks}
}

// anthropicTools 把 OpenAI function tools 转成 Anthropic tools（input_schema）。
func anthropicTools(list []any) []any {
	out := make([]any, 0, len(list))
	for _, raw := range list {
		t, _ := raw.(map[string]any)
		if t == nil {
			continue
		}
		fn, _ := t["function"].(map[string]any)
		if fn == nil {
			continue
		}
		name, _ := fn["name"].(string)
		desc, _ := fn["description"].(string)
		schema, _ := fn["parameters"].(map[string]any)
		if schema == nil {
			schema = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out = append(out, map[string]any{
			"name":         name,
			"description":  desc,
			"input_schema": schema,
		})
	}
	return out
}

// anthropicToolChoice 映射 tool_choice；none/未知 → nil（省略，等价 auto）。
func anthropicToolChoice(v any) map[string]any {
	switch tc := v.(type) {
	case string:
		switch strings.ToLower(strings.TrimSpace(tc)) {
		case "auto":
			return map[string]any{"type": "auto"}
		case "required":
			return map[string]any{"type": "any"}
		}
	case map[string]any:
		typ, _ := tc["type"].(string)
		switch strings.ToLower(strings.TrimSpace(typ)) {
		case "auto":
			return map[string]any{"type": "auto"}
		case "required", "any":
			return map[string]any{"type": "any"}
		case "tool":
			name, _ := tc["name"].(string)
			if name == "" {
				if fn, ok := tc["function"].(map[string]any); ok {
					name, _ = fn["name"].(string)
				}
			}
			if name != "" {
				return map[string]any{"type": "tool", "name": name}
			}
		}
	}
	return nil
}

// contentText 提取 content 的纯文本（字符串或 text 分片拼接）。
func contentText(v any) string {
	switch c := v.(type) {
	case string:
		return c
	case []any:
		var b strings.Builder
		for _, raw := range c {
			if p, ok := raw.(map[string]any); ok {
				if s, ok := p["text"].(string); ok {
					b.WriteString(s)
				}
			}
		}
		return b.String()
	}
	return ""
}

// ── 响应侧：Anthropic SSE → OpenAI SSE ─────────────────────────────────────

// messagesToChatReader 把上游 Anthropic SSE 转成 OpenAI chat.completion.chunk 流。
type messagesToChatReader struct {
	src     *bufio.Reader
	out     bytes.Buffer
	id      string
	model   string
	done    bool
	events  int // 已输出的事件数（用于 created 只取一次）
	created int64
	started bool
	// Anthropic 的 content block index 把文本块也算进去，OpenAI 的 tool_calls.index
	// 只数工具调用；两者必须分开映射，否则「先文本后工具」会产出 index=1 的单条调用。
	toolIdx  map[int]int // Anthropic block index → OpenAI tool index
	nextTool int
	// 用量：input/cache 在 message_start 里给，output_tokens 在 message_delta 里累加。
	inTokens     float64
	outTokens    float64
	cachedTokens float64
}

// newMessagesToChatReader 构造转换器。
func newMessagesToChatReader(r io.Reader, model string) io.ReadCloser {
	return &messagesToChatReader{src: bufio.NewReaderSize(r, 64*1024), model: model}
}

// Read 按需解析上游事件，产出 OpenAI SSE 字节。
func (m *messagesToChatReader) Read(p []byte) (int, error) {
	for m.out.Len() == 0 {
		if m.done {
			return 0, io.EOF
		}
		if err := m.pump(); err != nil {
			if err == io.EOF {
				// 上游没发 message_stop 就结束：补一个收尾，避免客户端判截断。
				m.emitDone()
				continue
			}
			return 0, err
		}
	}
	return m.out.Read(p)
}

// Close 关闭底层（本转换器不持有真实连接，由调用方的 body 负责；这里 no-op）。
func (m *messagesToChatReader) Close() error { return nil }

// pump 读取并处理一个 SSE 事件；没产出字节时继续。
func (m *messagesToChatReader) pump() error {
	var data string
	for {
		line, err := m.src.ReadString('\n')
		trimmed := strings.TrimRight(line, "\r\n")
		switch {
		case strings.HasPrefix(trimmed, "data:"):
			data = strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
		case trimmed == "":
			if data != "" {
				m.handle(data)
				return nil
			}
		}
		if err != nil {
			if data != "" {
				m.handle(data)
				if m.done {
					return io.EOF
				}
			}
			return io.EOF
		}
	}
}

// handle 处理一个 Anthropic 事件。
func (m *messagesToChatReader) handle(data string) {
	var ev map[string]any
	if err := json.Unmarshal([]byte(data), &ev); err != nil {
		return
	}
	typ, _ := ev["type"].(string)
	switch typ {
	case "message_start":
		if msg, ok := ev["message"].(map[string]any); ok {
			m.id, _ = msg["id"].(string)
			if mv, ok := msg["model"].(string); ok && mv != "" {
				m.model = mv
			}
			if u, ok := msg["usage"].(map[string]any); ok {
				m.inTokens = asFloat(u["input_tokens"])
				m.cachedTokens = asFloat(u["cache_read_input_tokens"])
			}
			m.created = 0
		}
		m.emitRole()
	case "content_block_start":
		idx := int(asFloat(ev["index"]))
		block, _ := ev["content_block"].(map[string]any)
		if block == nil {
			return
		}
		switch block["type"] {
		case "tool_use":
			id, _ := block["id"].(string)
			name, _ := block["name"].(string)
			m.emitToolStart(idx, id, name)
		case "text":
			// 文本在 delta 里给，start 只带空串。
		}
	case "content_block_delta":
		idx := int(asFloat(ev["index"]))
		d, _ := ev["delta"].(map[string]any)
		if d == nil {
			return
		}
		switch d["type"] {
		case "text_delta":
			if s, ok := d["text"].(string); ok && s != "" {
				m.emitDelta(map[string]any{"content": s})
			}
		case "thinking_delta":
			if s, ok := d["thinking"].(string); ok && s != "" {
				m.emitDelta(map[string]any{"reasoning_content": s})
			}
		case "input_json_delta":
			if s, ok := d["partial_json"].(string); ok && s != "" {
				m.emitToolArgs(idx, s)
			}
		}
	case "message_delta":
		// 真实上游把 input/cache 放在 message_start，output 放在这里；两者都给时以
		// 非零值为准，避免某侧省略 input_tokens 时统计归零。
		if u, ok := ev["usage"].(map[string]any); ok {
			if v := asFloat(u["output_tokens"]); v > 0 {
				m.outTokens = v
			}
			if v := asFloat(u["input_tokens"]); v > 0 {
				m.inTokens = v
			}
			if v := asFloat(u["cache_read_input_tokens"]); v > 0 {
				m.cachedTokens = v
			}
		}
		d, _ := ev["delta"].(map[string]any)
		reason, _ := d["stop_reason"].(string)
		m.emitFinish(anthropicStopToOpenAI(reason))
	case "message_stop":
		m.emitDone()
	case "error":
		if e, ok := ev["error"].(map[string]any); ok {
			if b, err := json.Marshal(map[string]any{"error": e}); err == nil {
				m.out.WriteString("data: ")
				m.out.Write(b)
				m.out.WriteString("\n\n")
			}
		}
		m.emitDone()
	}
}

func asFloat(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case json.Number:
		f, _ := x.Float64()
		return f
	}
	return 0
}

// anthropicStopToOpenAI 映射 stop_reason。
func anthropicStopToOpenAI(s string) string {
	switch s {
	case "end_turn", "stop_sequence":
		return "stop"
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	case "refusal":
		return "content_filter"
	}
	return "stop"
}

// chunk 组装一帧 OpenAI chunk 并写入 out。
func (m *messagesToChatReader) chunk(delta map[string]any, finish any, usage any) {
	created := m.created
	if created == 0 {
		created = 1
	}
	id := m.id
	if id == "" {
		id = "chatcmpl-messages"
	}
	obj := map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   m.model,
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         delta,
			"finish_reason": finish,
		}},
	}
	if usage != nil {
		obj["usage"] = usage
	}
	b, err := json.Marshal(obj)
	if err != nil {
		return
	}
	m.out.WriteString("data: ")
	m.out.Write(b)
	m.out.WriteString("\n\n")
}

func (m *messagesToChatReader) emitRole() {
	if m.started {
		return
	}
	m.started = true
	m.chunk(map[string]any{"role": "assistant", "content": ""}, nil, nil)
}

func (m *messagesToChatReader) emitDelta(delta map[string]any) {
	m.emitRole()
	m.chunk(delta, nil, nil)
}

func (m *messagesToChatReader) emitToolStart(blockIdx int, id, name string) {
	m.emitRole()
	if m.toolIdx == nil {
		m.toolIdx = map[int]int{}
	}
	idx := m.nextTool
	m.nextTool++
	m.toolIdx[blockIdx] = idx
	m.chunk(map[string]any{"tool_calls": []any{map[string]any{
		"index": idx,
		"id":    id,
		"type":  "function",
		"function": map[string]any{
			"name":      name,
			"arguments": "",
		},
	}}}, nil, nil)
}

func (m *messagesToChatReader) emitToolArgs(blockIdx int, args string) {
	idx, ok := m.toolIdx[blockIdx]
	if !ok {
		return
	}
	m.chunk(map[string]any{"tool_calls": []any{map[string]any{
		"index":    idx,
		"function": map[string]any{"arguments": args},
	}}}, nil, nil)
}

func (m *messagesToChatReader) emitFinish(reason string) {
	m.emitRole()
	m.chunk(map[string]any{}, reason, m.usage())
}

func (m *messagesToChatReader) emitDone() {
	if m.done {
		return
	}
	m.done = true
	m.out.WriteString("data: [DONE]\n\n")
}

// usage 组装 OpenAI usage：input/cache 来自 message_start，output 来自 message_delta。
func (m *messagesToChatReader) usage() any {
	if m.inTokens == 0 && m.outTokens == 0 && m.cachedTokens == 0 {
		return nil
	}
	res := map[string]any{
		"prompt_tokens":     m.inTokens,
		"completion_tokens": m.outTokens,
		"total_tokens":      m.inTokens + m.outTokens,
	}
	if m.cachedTokens > 0 {
		res["prompt_tokens_details"] = map[string]any{"cached_tokens": m.cachedTokens}
	}
	return res
}

// modelOfBody 从已解析请求体取 model（转换失败返回空串）。
func modelOfBody(body []byte) string {
	var probe struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &probe)
	return probe.Model
}

// chatMessagesStream 用 Anthropic Messages 路由发一次请求，并把响应转回 OpenAI SSE。
// 返回语义与 ChatStreamContext 一致：成功时 rc 是 OpenAI SSE 流，失败时 (status, raw)。
func (c *Client) chatMessagesStream(ctx context.Context, a *auth.Auth, body []byte, clientIP string, meta ChatMeta) (io.ReadCloser, int, []byte, error) {
	anthropicBody, err := buildMessagesBody(body)
	if err != nil {
		return nil, 0, nil, err
	}
	client, cerr := c.clientFor(a, true)
	if cerr != nil {
		return nil, 0, nil, cerr
	}
	url := c.chatBase(a) + messagesPath
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(anthropicBody))
	if err != nil {
		return nil, 0, nil, err
	}
	c.ChatHeaders(req, a, clientIP, meta)
	// Anthropic 协议头与流式 Accept（ChatHeaders 默认 application/json）。
	req.Header.Set("anthropic-version", anthropicVersion)
	req.Header.Set("Accept", "text/event-stream")
	reqCtx, cancel := context.WithCancel(ctx)
	req = req.WithContext(reqCtx)
	resp, err := client.Do(req)
	if err != nil {
		cancel()
		log.Printf("ERR: [upstream] messages acct=%s: transport error: %v", logfmt.Label(a.UID, a.Nickname), err)
		roundTripCloseIdle(client.Transport)
		return nil, 0, nil, err
	}
	if resp.StatusCode >= 400 {
		rb, derr := decompressBody(resp)
		if derr != nil {
			cancel()
			return nil, 0, nil, fmt.Errorf("decompress body: %w", derr)
		}
		raw, rerr := io.ReadAll(io.LimitReader(rb, 1<<20))
		rb.Close()
		cancel()
		if rerr != nil {
			return nil, 0, nil, fmt.Errorf("read body: %w", rerr)
		}
		kind := Classify(resp.StatusCode, string(raw))
		return nil, resp.StatusCode, raw, &Error{Kind: kind, Status: resp.StatusCode, Msg: string(raw)}
	}
	rb, derr := decompressBody(resp)
	if derr != nil {
		cancel()
		return nil, 0, nil, fmt.Errorf("decompress body: %w", derr)
	}
	conv := newMessagesToChatReader(rb, modelOfBody(body))
	return &messagesBody{conv: conv, raw: rb, cancel: cancel}, http.StatusOK, nil, nil
}

// messagesBody 把转换器与底层 body、请求 ctx 绑在一起：handler 关闭时一并释放。
type messagesBody struct {
	conv   io.ReadCloser
	raw    io.ReadCloser
	cancel context.CancelFunc
}

func (m *messagesBody) Read(p []byte) (int, error) { return m.conv.Read(p) }

func (m *messagesBody) Close() error {
	err := m.conv.Close()
	if m.raw != nil {
		_ = m.raw.Close()
	}
	if m.cancel != nil {
		m.cancel()
	}
	return err
}
