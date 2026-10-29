package upstream

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
)

// channelBody 渠道校验封套（让 /v2 判 ErrAccountFault → 触发换路径）。
const channelBody = `{"code":"11-128","msg":"request illegal, channel verification failed"}`

// issue #245：/v2 判渠道校验、回退 /console 之后拿到的 11102 不能升级成 model_blocked。
//
// 现场：15 个号跑几小时后全部软冷却、所有请求 503，日志里 11-128 与 11102 混在一起。
// 11102 走 BlockModelBackoff（负缓存 TTL 2 分钟起），于是「换接口重试」这一步的净效果
// 变成「把账号锁在这个模型上」——几个号轮转过去，整池就红了。
//
// 回退路径上的 11102 只说明「这条路径没有这个模型」：我们是因为 /v2 判了渠道校验才
// 换过去的，/console 本来就可能不提供这个模型，这不构成「该账号缺这个模型」的证据。
//
// 报文口径（本批同步）：对外返回**主路径 /v2 的 11-128 原文**，而不是 /console 的
// 11102。为什么：回退路径只是「换条小道试试」，它没有这个模型是**预期内**的，不是
// 新的诊断结论；照它报「模型不存在」会让运维和客户端都以为是模型下线，而真实原因
// 是被渠道校验挡住 —— 排障方向直接被带偏。Kind 仍是 ErrFallbackModelMissing（不是
// 主路径的 ErrAccountFault），handler 的 applyErrorPolicy 对它零动作，罚号策略不变。
func TestConsoleFallbackModelMissingIsRequestLevel(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		if strings.Contains(r.URL.Path, "/console/chat/completions") {
			_, _ = w.Write([]byte(`{"code":11102,"msg":"model [deepseek-v4.1-flash] service info not found"}`))
			return
		}
		_, _ = w.Write([]byte(channelBody))
	}))
	defer srv.Close()

	c := New()
	c.ChatBaseCN = srv.URL
	acct := &auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}
	_, status, raw, err := c.ChatStream(acct,
		[]byte(`{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`), "", ChatMeta{})
	if status != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", status, raw)
	}
	ue, ok := err.(*Error)
	if !ok {
		t.Fatalf("应返回分类信封，got %T (%v)", err, err)
	}
	if ue.Kind != ErrFallbackModelMissing {
		t.Fatalf("回退路径的 11102 应归请求级（ErrFallbackModelMissing），got %v", ue.Kind)
	}
	// 报文说真话：对外是主路径的渠道校验原文，不是 /console 的「模型不存在」。
	if !strings.Contains(string(raw), "channel verification") {
		t.Fatalf("应返回主路径（/v2）的渠道校验原文，got %s", raw)
	}
	if strings.Contains(string(raw), "service info not found") {
		t.Fatalf("不能把 /console 的 11102 报文透给客户端（会把排障带偏），got %s", raw)
	}
	if !strings.Contains(ue.Msg, "channel verification") {
		t.Fatalf("错误信封的 Msg 也应取主路径原文，got %q", ue.Msg)
	}
	if len(paths) != 2 || !strings.Contains(paths[0], "/v2/") || !strings.Contains(paths[1], "/console/") {
		t.Fatalf("应当先在 /v2 被拒再换 /console，实际路径=%v", paths)
	}
}

// 主路径自己报 11102 仍是 (账号,模型) 负缓存：那是「该后端确实没有这个模型」的
// 确定性答复，与回退路径的语义不同。这条用例防止把降级写宽了。
func TestPrimaryPathModelMissingStaysModelBlocked(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":11102,"msg":"model [glm-5.2] service info not found"}`))
	}))
	defer srv.Close()

	c := New()
	c.ChatBaseCN = srv.URL
	acct := &auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}
	_, _, _, err := c.ChatStream(acct,
		[]byte(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`), "", ChatMeta{})
	ue, ok := err.(*Error)
	if !ok {
		t.Fatalf("应返回分类信封，got %T (%v)", err, err)
	}
	if ue.Kind != ErrModelBlocked {
		t.Fatalf("主路径的 11102 应保持 ErrModelBlocked，got %v", ue.Kind)
	}
}
