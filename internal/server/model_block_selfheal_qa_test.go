package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"workbuddy2api/internal/auth"
)

// 本文件是「11102 全池自愈」的有界性护栏（与 model_block_selfheal_test.go 互补：
// 那里钉「该自愈时确实自愈 / 不该清 6004」，这里钉「自愈的量」与「轮换名额不被
// 自愈吞掉」两件事）。

// chatWithModel 用单次 POST /v1/chat/completions 打一发，返回响应记录器。
func chatWithModel(t *testing.T, h *Handler, model string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(
		`{"model":"`+model+`","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	return rec
}

// TestModelBlockSelfHealExactCallBound 上游持续 11102 时，自愈的总量必须**精确**是
// 「清一次 + 每个号最多试一次」：
//
//   - 单账号 + 死模型 → 每个请求恰 1 次上游调用（清一次 → 重试一次 → 负缓存被写回
//     → 收口）。这条把「只清一次」钉成精确值：若自愈被写成每轮都清，调用次数会翻倍。
//   - 第二个请求同样恰 1 次：负缓存被写回但**每次请求都会重新自愈一次**（设计如此，
//     见 handler 的 modelBlocksCleared 注释）——这里钉的是「每请求有界」，不是
//     「跨请求不再试」。跨请求的形态变化（本批改动后负缓存不再抑制重试）见测试尾部注释。
func TestModelBlockSelfHealExactCallBound(t *testing.T) {
	cn1 := &auth.Auth{UID: "cn1", AccessToken: "at-cn1", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"}
	p := testPoolWith(cn1)
	p.BlockModelBackoff("cn1", selfHealModel, "11102 model not available")

	var calls int32
	up := newSelfHealUpstream(t, &calls, http.StatusNotFound,
		`{"code":11102,"msg":"model [`+selfHealModel+`] service info not found"}`)
	h := NewHandler(Config{Pool: p, Upstream: up, MaxRotate: 4})

	rec := chatWithModel(t, h, selfHealModel)
	if rec.Code == http.StatusOK {
		t.Fatalf("模型确实不存在时不得报成功: %s", rec.Body.String())
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("首个请求上游调用=%d want 恰好 1（清一次 + 试一次）", got)
	}
	// 负缓存被写回：否则下一个请求仍会「每次清一次」（缓存完全失效）。
	if st := p.ModelBlocked(selfHealModel); !st.Blocked || st.Count != 1 {
		t.Fatalf("失败后负缓存应被写回: %+v", st)
	}

	before := atomic.LoadInt32(&calls)
	rec2 := chatWithModel(t, h, selfHealModel)
	if rec2.Code == http.StatusOK {
		t.Fatalf("第二个请求同样不得报成功: %s", rec2.Body.String())
	}
	if delta := atomic.LoadInt32(&calls) - before; delta != 1 {
		t.Fatalf("第二个请求上游调用=%d want 恰好 1（每请求有界）", delta)
	}
}

// TestModelBlockSelfHealWorksWithMaxRotateOne 自愈的重试不得吃掉唯一的轮换名额。
//
// 症状（修复前实测）：max_rotate=1 的部署里，负缓存全挡时自愈**清掉了**缓存、
// 一次上游都没打（continue 用掉唯一的循环名额 → 循环结束）、对外仍是
// 503 no_healthy_account。净效果是「缓存白丢 + 请求白失败」：下一个请求重复同一
// 形态，而池子在整个过程中没有任何一次真实探测，模型恢复了也不知道。
//
// 修法：自愈的 continue 前回退索引（i--），让「同号重试」不占轮换名额 ——
// 与 upstream 里「同路径去压缩重试」的做法一致；有界性由 modelBlocksCleared 保证
// （每请求至多清一次），不会变成无限循环。
func TestModelBlockSelfHealWorksWithMaxRotateOne(t *testing.T) {
	cn1 := &auth.Auth{UID: "cn1", AccessToken: "at-cn1", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"}
	p := testPoolWith(cn1)
	p.BlockModelBackoff("cn1", selfHealModel, "11102 model not available")

	var calls int32
	h := NewHandler(Config{Pool: p, Upstream: newSelfHealUpstream(t, &calls, http.StatusOK, sseOK), MaxRotate: 1})

	rec := chatWithModel(t, h, selfHealModel)
	if rec.Code != http.StatusOK {
		t.Fatalf("max_rotate=1 也应完成「清缓存 + 重试」，got code=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("上游调用=%d want 1（自愈重试不该被轮换名额吞掉）", got)
	}
	// 成功后负缓存必须是空的（重试成功即自愈完成）。
	if st := p.ModelBlocked(selfHealModel); st.Blocked {
		t.Fatalf("自愈成功后负缓存应清空: %+v", st)
	}
}
