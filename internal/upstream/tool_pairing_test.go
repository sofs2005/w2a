package upstream

import (
	"encoding/json"
	"reflect"
	"testing"
)

// TestCleanupOrphanToolCallsNoTraffic 无工具流量 → 零改动（changed=false）。
func TestCleanupOrphanToolCallsNoTraffic(t *testing.T) {
	messages := []any{
		map[string]any{"role": "system", "content": "hi"},
		map[string]any{"role": "user", "content": "hello"},
		map[string]any{"role": "assistant", "content": "hi there"},
	}
	out, changed := cleanupOrphanToolCalls(messages)
	if changed {
		t.Fatal("no-traffic should be unchanged")
	}
	if &out[0] != &messages[0] {
		t.Fatal("no-traffic should return the original slice")
	}
}

// TestCleanupOrphanToolCallWithoutResult 孤儿 tool_call（无结果）→ 删除 tool_calls 键。
func TestCleanupOrphanToolCallWithoutResult(t *testing.T) {
	messages := []any{
		map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{
			map[string]any{"id": "call_1", "type": "function", "function": map[string]any{"name": "Grep", "arguments": "{}"}},
		}},
		map[string]any{"role": "user", "content": "continue"},
	}
	out, changed := cleanupOrphanToolCalls(messages)
	if !changed {
		t.Fatal("orphan tool_call should be cleaned")
	}
	asst := out[0].(map[string]any)
	if _, ok := asst["tool_calls"]; ok {
		t.Fatalf("tool_calls should be removed, got %#v", asst)
	}
}

// TestCleanupOrphanPartialBatch 一批两个 tool_call，只有 c1 拿到结果 → 按 keepCalls
// 对称裁剪：调用侧只留 c1、无结果的 c2 被剔，两侧不残留半截配对。
func TestCleanupOrphanPartialBatch(t *testing.T) {
	messages := []any{
		map[string]any{"role": "assistant", "tool_calls": []any{
			map[string]any{"id": "c1", "type": "function", "function": map[string]any{"name": "read", "arguments": "{}"}},
			map[string]any{"id": "c2", "type": "function", "function": map[string]any{"name": "read", "arguments": "{}"}},
		}},
		map[string]any{"role": "tool", "tool_call_id": "c1", "content": "ok"},
		map[string]any{"role": "user", "content": "next"},
	}
	out, changed := cleanupOrphanToolCalls(messages)
	if !changed {
		t.Fatal("partial batch should be cleaned")
	}
	asst := out[0].(map[string]any)
	tcs, ok := asst["tool_calls"].([]any)
	if !ok || len(tcs) != 1 {
		t.Fatalf("只应保留有结果的 c1，实际 %#v", asst)
	}
	if id, _ := tcs[0].(map[string]any)["id"].(string); id != "c1" {
		t.Fatalf("保留的应是 c1，实际 %s", id)
	}
	// 关键不变式：出站任何 tool 结果都必须有对应 tool_call，任何 tool_call 都必须有
	// 结果——否则上游判 11148（tool calls and tool results do not match）。
	assertPairingSymmetric(t, out)
}

// assertPairingSymmetric 断言出站消息两侧配对对称：每个 tool_call id 都有结果，
// 每个 tool 结果的 id 都有调用。task 要求「只留有结果配对的调用、孤儿结果整删，
// 两侧同口径」——该断言是两侧同口径的直接表达。
func assertPairingSymmetric(t *testing.T, msgs []any) {
	t.Helper()
	allCalls := map[string]bool{}
	for _, m := range msgs {
		mm, _ := m.(map[string]any)
		if role, _ := mm["role"].(string); role != "assistant" {
			continue
		}
		tcs, ok := mm["tool_calls"].([]any)
		if !ok {
			continue
		}
		for _, tci := range tcs {
			tc, _ := tci.(map[string]any)
			if id, _ := tc["id"].(string); id != "" {
				allCalls[id] = true
			}
		}
	}
	resultIDs := map[string]bool{}
	for _, m := range msgs {
		mm, _ := m.(map[string]any)
		if role, _ := mm["role"].(string); role != "tool" {
			continue
		}
		id, _ := mm["tool_call_id"].(string)
		resultIDs[id] = true
		if !allCalls[id] {
			t.Fatalf("残留孤儿 tool 结果 %s——上游会判 11148：%#v", id, msgs)
		}
	}
	for id := range allCalls {
		if !resultIDs[id] {
			t.Fatalf("残留无结果 tool_call %s——上游会判 11148：%#v", id, msgs)
		}
	}
}

// TestCleanupOrphanResultOnly 孤儿 tool 结果（无对应 tool_call）→ 整条消息删除。
func TestCleanupOrphanResultOnly(t *testing.T) {
	messages := []any{
		map[string]any{"role": "user", "content": "hi"},
		map[string]any{"role": "tool", "tool_call_id": "ghost", "content": "orphan"},
	}
	out, changed := cleanupOrphanToolCalls(messages)
	if !changed {
		t.Fatal("orphan result should be cleaned")
	}
	if len(out) != 1 || out[0].(map[string]any)["role"] != "user" {
		t.Fatalf("orphan tool message should be removed, got %#v", out)
	}
}

// TestCleanupOrphanPairingPreserved 正例零改动：完整配对的多 tool_call 轮 + 正常文本轮，
// 所有字段原样保留。
func TestCleanupOrphanPairingPreserved(t *testing.T) {
	grepArgs := `{"pattern":"foo","path":"."}`
	readArgs := `{"file_path":"a.go"}`
	messages := []any{
		map[string]any{"role": "user", "content": "search"},
		map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{
			map[string]any{"id": "call_1", "type": "function", "function": map[string]any{"name": "Grep", "arguments": grepArgs}},
			map[string]any{"id": "call_2", "type": "function", "function": map[string]any{"name": "Read", "arguments": readArgs}},
		}},
		map[string]any{"role": "tool", "tool_call_id": "call_1", "content": "3 matches"},
		map[string]any{"role": "tool", "tool_call_id": "call_2", "content": "file body"},
		map[string]any{"role": "user", "content": "keep going"},
	}
	out, changed := cleanupOrphanToolCalls(messages)
	if changed {
		t.Fatal("fully paired round-trip must be zero-change")
	}
	if len(out) != 5 {
		t.Fatalf("message count changed: %d", len(out))
	}
	asst := out[1].(map[string]any)
	tcs := asst["tool_calls"].([]any)
	if len(tcs) != 2 {
		t.Fatalf("tool_calls dropped from paired round-trip: %#v", asst)
	}
	// 函数参数原样保留（引用不变）。
	fn := tcs[0].(map[string]any)["function"].(map[string]any)
	if fn["arguments"] != grepArgs {
		t.Fatalf("call_1 arguments mutated: %v", fn["arguments"])
	}
}

// TestCleanupOrphanOutOfOrderToolBeforeResult 乱序：tool 结果消息出现在 assistant
// tool_call 之前（不按顺序但 id 齐全）→ 仍保留（按 id 集合配对，与顺序无关）。
func TestCleanupOrphanOutOfOrderToolBeforeResult(t *testing.T) {
	messages := []any{
		map[string]any{"role": "tool", "tool_call_id": "call_9", "content": "res"},
		map[string]any{"role": "assistant", "tool_calls": []any{
			map[string]any{"id": "call_9", "type": "function", "function": map[string]any{"name": "f", "arguments": "{}"}},
		}},
	}
	out, changed := cleanupOrphanToolCalls(messages)
	if changed {
		t.Fatal("id-complete out-of-order pairing must be preserved")
	}
	if len(out) != 2 {
		t.Fatalf("message count changed: %d", len(out))
	}
}

// TestCleanupOrphanDuplicateID 重复 tool_call id（两处引用同一结果 id）：
// 结果侧唯一、调用侧重复——每次按集合取并，保持「id 命中结果即保留」的最宽口径。
func TestCleanupOrphanDuplicateID(t *testing.T) {
	messages := []any{
		map[string]any{"role": "tool", "tool_call_id": "dup", "content": "r"},
		map[string]any{"role": "assistant", "tool_calls": []any{
			map[string]any{"id": "dup", "type": "function", "function": map[string]any{"name": "a", "arguments": "{}"}},
		}},
		map[string]any{"role": "assistant", "tool_calls": []any{
			map[string]any{"id": "dup", "type": "function", "function": map[string]any{"name": "b", "arguments": "{}"}},
		}},
	}
	_, changed := cleanupOrphanToolCalls(messages)
	if changed {
		t.Fatal("duplicate id referencing an existing result must be preserved (widest keep)")
	}
}

// TestCleanupOrphanThroughPrepareBody 集成：孤儿 tool_call 经 PrepareBodyOpt 全链路被剔除，
// 且与消息顺序无关。
func TestCleanupOrphanThroughPrepareBody(t *testing.T) {
	body := `{"model":"glm-5.2","messages":[
		{"role":"assistant","tool_calls":[{"id":"bad","type":"function","function":{"name":"f","arguments":"{}"}}]},
		{"role":"user","content":"hi"}
	]}`
	out := PrepareBodyOpt([]byte(body), false)
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	msgs := obj["messages"].([]any)
	asst := msgs[0].(map[string]any)
	if _, ok := asst["tool_calls"]; ok {
		t.Fatalf("orphan tool_calls should be stripped by PrepareBodyOpt, got %#v", asst)
	}
}

// TestRepackToolResultBlocksInsertedNotice 复刻真实会话的卡死结构：Codex 的
// <image_resize_notice> 作为 developer 消息插在并行 tool 结果中间，上游判
// 11148 tool_call_sequence_broken。修复后结果必须连续、插入物后移、内容不变。
func TestRepackToolResultBlocksInsertedNotice(t *testing.T) {
	notice := "<image_resize_notice>resized</image_resize_notice>"
	messages := []any{
		map[string]any{"role": "assistant", "tool_calls": []any{
			map[string]any{"id": "c00", "type": "function", "function": map[string]any{"name": "view_image", "arguments": "{}"}},
			map[string]any{"id": "c01", "type": "function", "function": map[string]any{"name": "view_image", "arguments": "{}"}},
		}},
		map[string]any{"role": "tool", "tool_call_id": "c00", "content": "img0"},
		map[string]any{"role": "developer", "content": notice},
		map[string]any{"role": "tool", "tool_call_id": "c01", "content": "img1"},
		map[string]any{"role": "user", "content": "next"},
	}
	out, changed := repackToolResultBlocks(messages)
	if !changed {
		t.Fatal("插入物应触发重排")
	}
	if len(out) != 5 {
		t.Fatalf("消息数不应变化，实际 %d", len(out))
	}
	// 只调顺序不改内容：developer 消息原 map 对象后移（引用不变，深比较外的同一实例）。
	if out[3].(map[string]any)["content"] != notice {
		t.Fatalf("插入物内容被改动：%#v", out[3])
	}
	// reflect.ValueOf 同 interface 值的 Pointer 比较同一 map 实例（&out[3] != &messages[2]
	// 只是比较 slice 元素地址，恒不等，不能这么断言）。
	if reflect.ValueOf(out[3]).Pointer() != reflect.ValueOf(messages[2]).Pointer() {
		t.Fatalf("插入物应是原 map 对象（只调顺序不改内容）：%#v", out[3])
	}
	// 顺序：assistant 之后紧跟两条 tool 结果，developer 被移到其后。
	wantRoles := []string{"assistant", "tool", "tool", "developer", "user"}
	for i, w := range wantRoles {
		got, _ := out[i].(map[string]any)["role"].(string)
		if got != w {
			t.Fatalf("out[%d] 角色应为 %s，实际 %s（%#v）", i, w, got, out)
		}
	}
	// 结果顺序保持 c00 -> c01。
	if id, _ := out[1].(map[string]any)["tool_call_id"].(string); id != "c00" {
		t.Fatalf("第一份结果应为 c00，实际 %s", id)
	}
	if id, _ := out[2].(map[string]any)["tool_call_id"].(string); id != "c01" {
		t.Fatalf("第二份结果应为 c01，实际 %s", id)
	}
}

// TestRepackToolResultBlocksNoInsert 无插入物（完整连续配对）→ 零改动（返回原 slice）。
func TestRepackToolResultBlocksNoInsert(t *testing.T) {
	messages := []any{
		map[string]any{"role": "assistant", "tool_calls": []any{
			map[string]any{"id": "c1", "type": "function", "function": map[string]any{"name": "f", "arguments": "{}"}},
		}},
		map[string]any{"role": "tool", "tool_call_id": "c1", "content": "r"},
		map[string]any{"role": "user", "content": "n"},
	}
	out, changed := repackToolResultBlocks(messages)
	if changed {
		t.Fatal("完整配对不应改动")
	}
	if &out[0] != &messages[0] {
		t.Fatal("零改动应返回原 slice")
	}
}

// TestRepackToolResultBlocksNextGroupHeadNotSwallowed 回归（真实会话 msg[181] 形态）：
// 下一组 assistant.tool_calls 紧跟上一组结果时，绝不能被上一组的收集循环当「插入物」
// 吞掉——否则它自己那批结果永远得不到重排，上游照旧判 11148。
func TestRepackToolResultBlocksNextGroupHeadNotSwallowed(t *testing.T) {
	msgs := []any{
		map[string]any{"role": "user", "content": "go"},
		// 第 1 组：exec_command ×2（结果连续，无插入物）。
		map[string]any{"role": "assistant", "content": "", "tool_calls": []any{
			map[string]any{"id": "c00", "type": "function", "function": map[string]any{"name": "exec_command", "arguments": "{}"}},
			map[string]any{"id": "c01", "type": "function", "function": map[string]any{"name": "exec_command", "arguments": "{}"}},
		}},
		map[string]any{"role": "tool", "tool_call_id": "c00", "content": "ok0"},
		map[string]any{"role": "tool", "tool_call_id": "c01", "content": "ok1"},
		// 第 2 组：view_image ×2，紧邻上一组结果，且自身结果被 notice 打断。
		map[string]any{"role": "assistant", "content": "", "tool_calls": []any{
			map[string]any{"id": "c10", "type": "function", "function": map[string]any{"name": "view_image", "arguments": "{}"}},
			map[string]any{"id": "c11", "type": "function", "function": map[string]any{"name": "view_image", "arguments": "{}"}},
		}},
		map[string]any{"role": "tool", "tool_call_id": "c10", "content": "img0"},
		map[string]any{"role": "developer", "content": "<image_resize_notice>n"},
		map[string]any{"role": "tool", "tool_call_id": "c11", "content": "img1"},
		map[string]any{"role": "developer", "content": "<image_resize_notice>n"},
	}
	out, changed := repackToolResultBlocks(msgs)
	if !changed {
		t.Fatalf("changed=false，第二组未被重排")
	}
	if len(out) != len(msgs) {
		t.Fatalf("长度变化：%d -> %d", len(msgs), len(out))
	}
	// 期望顺序：[user][a1][tool c00][tool c01][a2][tool c10][tool c11][dev][dev]
	want := []string{"", "", "c00", "c01", "", "c10", "c11", "", ""}
	for i, m := range out {
		mm, _ := m.(map[string]any)
		id, _ := mm["tool_call_id"].(string)
		if id != want[i] {
			t.Fatalf("[%d] tool_call_id=%q，期望 %q；实际顺序 %v", i, id, want[i], repackSeqOf(out))
		}
	}
	assertRepackPairsContiguous(t, out)
}

// TestRepackToolResultBlocksThreeConsecutiveGroups 回归：连续多组、仅末组含插入物
// ——确保组头识别在连续场景下不退化。
func TestRepackToolResultBlocksThreeConsecutiveGroups(t *testing.T) {
	msgs := []any{
		map[string]any{"role": "assistant", "content": "", "tool_calls": []any{
			map[string]any{"id": "a0", "type": "function", "function": map[string]any{"name": "x", "arguments": "{}"}},
		}},
		map[string]any{"role": "tool", "tool_call_id": "a0", "content": "r"},
		map[string]any{"role": "assistant", "content": "", "tool_calls": []any{
			map[string]any{"id": "b0", "type": "function", "function": map[string]any{"name": "x", "arguments": "{}"}},
			map[string]any{"id": "b1", "type": "function", "function": map[string]any{"name": "x", "arguments": "{}"}},
		}},
		map[string]any{"role": "tool", "tool_call_id": "b0", "content": "r"},
		map[string]any{"role": "tool", "tool_call_id": "b1", "content": "r"},
		map[string]any{"role": "assistant", "content": "", "tool_calls": []any{
			map[string]any{"id": "c0", "type": "function", "function": map[string]any{"name": "x", "arguments": "{}"}},
			map[string]any{"id": "c1", "type": "function", "function": map[string]any{"name": "x", "arguments": "{}"}},
		}},
		map[string]any{"role": "tool", "tool_call_id": "c0", "content": "r"},
		map[string]any{"role": "developer", "content": "<notice>"},
		map[string]any{"role": "tool", "tool_call_id": "c1", "content": "r"},
		map[string]any{"role": "developer", "content": "<notice>"},
	}
	out, changed := repackToolResultBlocks(msgs)
	if !changed {
		t.Fatalf("changed=false")
	}
	if len(out) != len(msgs) {
		t.Fatalf("长度变化：%d -> %d", len(msgs), len(out))
	}
	assertRepackPairsContiguous(t, out)
}

func repackSeqOf(msgs []any) []string {
	var s []string
	for _, m := range msgs {
		mm, _ := m.(map[string]any)
		role, _ := mm["role"].(string)
		if id, _ := mm["tool_call_id"].(string); id != "" {
			s = append(s, role+":"+id)
		} else {
			s = append(s, role)
		}
	}
	return s
}

// assertRepackPairsContiguous 断言每个 assistant.tool_calls 的结果在其后连续出现。
func assertRepackPairsContiguous(t *testing.T, msgs []any) {
	t.Helper()
	for i := 0; i < len(msgs); i++ {
		mm, _ := msgs[i].(map[string]any)
		tcs, ok := mm["tool_calls"].([]any)
		if !ok || len(tcs) == 0 {
			continue
		}
		want := map[string]bool{}
		for _, tci := range tcs {
			tc, _ := tci.(map[string]any)
			if id, _ := tc["id"].(string); id != "" {
				want[id] = true
			}
		}
		got := map[string]bool{}
		j := i + 1
		for j < len(msgs) {
			nxt, _ := msgs[j].(map[string]any)
			if r, _ := nxt["role"].(string); r != "tool" {
				break
			}
			if id, _ := nxt["tool_call_id"].(string); id != "" {
				got[id] = true
			}
			j++
		}
		if len(got) != len(want) {
			t.Fatalf("assistant[%d] 结果不连续：want=%d got=%d 序列=%v", i, len(want), len(got), repackSeqOf(msgs))
		}
	}
}

// TestRepackThenCleanupRealShape 集成（真实会话卡死形态）：多 tool 并行 + 图片
// resize notice 插在结果中间，经 repack + cleanup 后出站载荷零违规——结果连续、
// 配对对称。走的是 PrepareBodyOpt 生产转换路径，而非只调内部函数。
func TestRepackThenCleanupRealShape(t *testing.T) {
	body := `{"model":"glm-5.2","messages":[
		{"role":"user","content":"go"},
		{"role":"assistant","content":"","tool_calls":[
			{"id":"c00","type":"function","function":{"name":"exec_command","arguments":"{}"}},
			{"id":"c01","type":"function","function":{"name":"exec_command","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"c00","content":"ok0"},
		{"role":"tool","tool_call_id":"c01","content":"ok1"},
		{"role":"assistant","content":"","tool_calls":[
			{"id":"i00","type":"function","function":{"name":"view_image","arguments":"{\"path\":\"a.png\"}"}},
			{"id":"i01","type":"function","function":{"name":"view_image","arguments":"{\"path\":\"b.png\"}"}}]},
		{"role":"tool","tool_call_id":"i00","content":"img0"},
		{"role":"developer","content":"<image_resize_notice>resized</image_resize_notice>"},
		{"role":"tool","tool_call_id":"i01","content":"img1"},
		{"role":"user","content":"next"}
	]}`
	out := PrepareBodyOpt([]byte(body), false)
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	msgs := obj["messages"].([]any)
	if len(msgs) != 9 {
		t.Fatalf("消息数不应变化，实际 %d", len(msgs))
	}
	// developer 归一为 system（normalizeRoles 既有行为），插在 notice 位次的它
	// 应已被挪到两条 tool 结果之后。
	seq := repackSeqOf(msgs)
	// i00/i01 两条结果连续（中间不再夹 developer/system）。
	idx := -1
	for i, s := range seq {
		if s == "tool:i00" {
			idx = i
			break
		}
	}
	if idx < 0 || idx+1 >= len(seq) || seq[idx+1] != "tool:i01" {
		t.Fatalf("i00/i01 结果应连续，实际序列 %v", seq)
	}
	// notice 消息在 i01 之后（后移到位）。
	if notice := msgs[idx+2].(map[string]any); notice["content"] != "<image_resize_notice>resized</image_resize_notice>" {
		t.Fatalf("插入物应在结果之后且内容不变，实际 %#v", notice)
	}
	assertRepackPairsContiguous(t, msgs)
	assertPairingSymmetric(t, msgs)
}

// TestRepackNoticeBeforeFirstResult 通知插在**首个**结果之前时也要后移。
//
// 这是上游补丁 0001 修的核心缺口：旧实现在 results 还为空时直接 break，通知被留在
// assistant 与首个结果之间，配对依旧断裂 → 上游判 11148 顶死整条会话。
func TestRepackNoticeBeforeFirstResult(t *testing.T) {
	call := map[string]any{"role": "assistant", "tool_calls": []any{
		map[string]any{"id": "c1", "type": "function", "function": map[string]any{"name": "f", "arguments": "{}"}},
	}}
	notice := map[string]any{"role": "developer", "content": "URI guard"}
	result := map[string]any{"role": "tool", "tool_call_id": "c1", "content": "ok"}
	next := map[string]any{"role": "user", "content": "next"}

	out, changed := repackToolResultBlocks([]any{call, notice, result, next})
	if !changed {
		t.Fatal("通知插在首个结果之前应触发重排")
	}
	if len(out) != 4 ||
		reflect.ValueOf(out[0]).Pointer() != reflect.ValueOf(call).Pointer() ||
		reflect.ValueOf(out[1]).Pointer() != reflect.ValueOf(result).Pointer() ||
		reflect.ValueOf(out[2]).Pointer() != reflect.ValueOf(notice).Pointer() ||
		reflect.ValueOf(out[3]).Pointer() != reflect.ValueOf(next).Pointer() {
		t.Fatalf("顺序/内容不符：%#v", out)
	}
}

// TestRepackNoticeAndTurnBoundaries 通知可跨首个结果与并行结果之间收集，
// 但绝不跨越 user / assistant 轮边界。
func TestRepackNoticeAndTurnBoundaries(t *testing.T) {
	call := map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "c1"}, map[string]any{"id": "c2"}}}
	dev := map[string]any{"role": "developer", "content": "dev"}
	sys := map[string]any{"role": "system", "content": "system"}
	r1 := map[string]any{"role": "tool", "tool_call_id": "c1", "content": "one"}
	r2 := map[string]any{"role": "tool", "tool_call_id": "c2", "content": "two"}
	user := map[string]any{"role": "user", "content": "boundary"}
	out, changed := repackToolResultBlocks([]any{call, dev, r1, sys, r2, user})
	want := []any{call, r1, r2, dev, sys, user}
	if !changed || !reflect.DeepEqual(out, want) {
		t.Fatalf("changed=%v out=%#v want=%#v", changed, out, want)
	}

	// user 轮边界之前的通知不得被搬到下一轮的结果组里。
	nextCall := map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "later"}}}
	laterResult := map[string]any{"role": "tool", "tool_call_id": "later", "content": "later"}
	input := []any{call, dev, user, r1, nextCall, laterResult}
	out, changed = repackToolResultBlocks(input)
	if changed || !reflect.DeepEqual(out, input) {
		t.Fatalf("必须保留轮边界：changed=%v out=%#v", changed, out)
	}
}

// TestRepackTrailingNoticeIsNoop 通知全在结果之后时零改动（不产生无谓重排）。
func TestRepackTrailingNoticeIsNoop(t *testing.T) {
	call := map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "c1"}}}
	result := map[string]any{"role": "tool", "tool_call_id": "c1", "content": "ok"}
	notice := map[string]any{"role": "developer", "content": "notice"}
	input := []any{call, result, notice}
	out, changed := repackToolResultBlocks(input)
	if changed || !reflect.DeepEqual(out, input) {
		t.Fatalf("尾部通知应为空操作：changed=%v out=%#v", changed, out)
	}
}

// ---------------------------------------------------------------------------
// mergeAdjacentToolCalls
// ---------------------------------------------------------------------------

// tcMsg 造一条带单个 tool_call 的 assistant（content 为 nil，与 agent 客户端回放
// 并行调用时的报文形状一致）。
func tcMsg(id string) map[string]any {
	return map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{
		map[string]any{"id": id, "type": "function", "function": map[string]any{"name": "f", "arguments": "{}"}},
	}}
}

// callIDsOf 取消息的 tool_calls id 序列（非 assistant / 无 tool_calls → nil）。
func callIDsOf(m any) []string {
	mm, _ := m.(map[string]any)
	if role, _ := mm["role"].(string); role != "assistant" {
		return nil
	}
	tcs, _ := mm["tool_calls"].([]any)
	var ids []string
	for _, tci := range tcs {
		tc, _ := tci.(map[string]any)
		if id, _ := tc["id"].(string); id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

// TestMergeAdjacentToolCallsSplitParallel 线上复现形态：同一批并行调用被拆成两条
// 紧邻的 assistant.tool_calls。上游 deepseek 系对「带 tool_calls 的 assistant 紧跟
// 另一条带 tool_calls 的 assistant」判 400 code=11148 并顶死整条会话（换号无效，
// 不是账号问题）——这是 mergeAdjacentToolCalls 的正面修复目标。
func TestMergeAdjacentToolCallsSplitParallel(t *testing.T) {
	messages := []any{
		tcMsg("c00"),
		tcMsg("c01"),
		map[string]any{"role": "tool", "tool_call_id": "c00", "content": "r0"},
		map[string]any{"role": "tool", "tool_call_id": "c01", "content": "r1"},
	}
	out, changed := mergeAdjacentToolCalls(messages)
	if !changed {
		t.Fatal("背靠背的两条 assistant.tool_calls 应被合并")
	}
	if len(out) != 3 {
		t.Fatalf("两条 assistant 应合成一条，实际 %d 条消息", len(out))
	}
	// 拼接顺序必须是声明顺序（= 结果的 wire 顺序），不得反转。
	if got := callIDsOf(out[0]); !reflect.DeepEqual(got, []string{"c00", "c01"}) {
		t.Fatalf("合并后 tool_calls = %v want [c00 c01]", got)
	}
	// 合并后不得再出现「相邻的两条带 tool_calls 的 assistant」。
	prevHadCalls := false
	for i, m := range out {
		hasCalls := len(callIDsOf(m)) > 0
		if prevHadCalls && hasCalls {
			t.Fatalf("消息[%d] 仍是相邻的两条 assistant.tool_calls（上游判 11148）：%#v", i, out)
		}
		prevHadCalls = hasCalls
	}
}

// TestMergeAdjacentToolCallsThreeConsecutive 三条连续全并（多路并行调用被拆成三条）。
func TestMergeAdjacentToolCallsThreeConsecutive(t *testing.T) {
	out, changed := mergeAdjacentToolCalls([]any{tcMsg("c00"), tcMsg("c01"), tcMsg("c02")})
	if !changed {
		t.Fatal("三条连续应被合并")
	}
	if len(out) != 1 {
		t.Fatalf("应合成一条，实际 %d", len(out))
	}
	if got := callIDsOf(out[0]); !reflect.DeepEqual(got, []string{"c00", "c01", "c02"}) {
		t.Fatalf("tool_calls = %v want [c00 c01 c02]", got)
	}
}

// TestMergeAdjacentToolCallsNoop 已是规范形态（一条 assistant 带全部 tool_calls）
// 必须零改动——上层依赖 changed=false 判断 no-op。
func TestMergeAdjacentToolCallsNoop(t *testing.T) {
	merged := map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{
		map[string]any{"id": "c00"}, map[string]any{"id": "c01"},
	}}
	messages := []any{
		merged,
		map[string]any{"role": "tool", "tool_call_id": "c00", "content": "r0"},
	}
	out, changed := mergeAdjacentToolCalls(messages)
	if changed {
		t.Error("规范形态不应被改动")
	}
	if &out[0] != &messages[0] {
		t.Error("零改动应返回原 slice")
	}
}

// TestMergeAdjacentToolCallsBoundaries 合并条件从严的边界：合并只能发生在**相邻**
// 且后一条无正文的 assistant.tool_calls 之间。隔着消息、后一条有正文、前一条无
// tool_calls 三种都不合并——凭猜测合并会改变语义。
func TestMergeAdjacentToolCallsBoundaries(t *testing.T) {
	t.Run("中间隔着 user 不合并", func(t *testing.T) {
		// 隔了消息说明不是同一批声明，这类形态交给 repackToolResultBlocks 原样处理。
		messages := []any{tcMsg("c00"), map[string]any{"role": "user", "content": "x"}, tcMsg("c01")}
		if _, changed := mergeAdjacentToolCalls(messages); changed {
			t.Error("非相邻不应合并")
		}
	})
	t.Run("后一条 content 非空不合并", func(t *testing.T) {
		// content 非空无法无损拼接，不猜语义。
		messages := []any{tcMsg("c00"), map[string]any{"role": "assistant", "content": "text",
			"tool_calls": []any{map[string]any{"id": "c01"}}}}
		if _, changed := mergeAdjacentToolCalls(messages); changed {
			t.Error("后一条有正文不应合并")
		}
	})
	t.Run("前一条无 tool_calls 不合并", func(t *testing.T) {
		messages := []any{map[string]any{"role": "assistant", "content": "hi"}, tcMsg("c00")}
		if _, changed := mergeAdjacentToolCalls(messages); changed {
			t.Error("前一条无 tool_calls 不应合并")
		}
	})
	t.Run("单条消息不越界", func(t *testing.T) {
		if _, changed := mergeAdjacentToolCalls([]any{tcMsg("c00")}); changed {
			t.Error("单条消息不应改动")
		}
	})
}

// TestMergeAdjacentToolCallsFoldTextBackward 反向拆分顺序：正文与调用声明拆成两条
// 独立 assistant（正文在后）。正文要折进前一条带 tool_calls 的 assistant，合成
// assistant(正文 + tool_calls)——这样「声明 tool_calls 的 assistant 紧跟它自己的
// 结果」在两种拆分顺序下都成立。
func TestMergeAdjacentToolCallsFoldTextBackward(t *testing.T) {
	messages := []any{
		tcMsg("c00"),
		map[string]any{"role": "assistant", "content": "我先看一下"},
		map[string]any{"role": "tool", "tool_call_id": "c00", "content": "r0"},
	}
	out, changed := mergeAdjacentToolCalls(messages)
	if !changed {
		t.Fatal("后置正文应折进前一条")
	}
	if len(out) != 2 {
		t.Fatalf("正文应折进上一条，实际 %d 条消息", len(out))
	}
	asst := out[0].(map[string]any)
	if asst["content"] != "我先看一下" {
		t.Fatalf("正文未折入：%#v", asst)
	}
	if got := callIDsOf(asst); !reflect.DeepEqual(got, []string{"c00"}) {
		t.Fatalf("tool_calls 丢失：%v", got)
	}
	// 数组正文可能含多模态块，拼接会丢结构——不折叠，原样出站。
	t.Run("数组正文不折叠", func(t *testing.T) {
		arr := []any{map[string]any{"type": "text", "text": "hi"}}
		msgs2 := []any{tcMsg("c00"), map[string]any{"role": "assistant", "content": arr}}
		if _, ch := mergeAdjacentToolCalls(msgs2); ch {
			t.Error("数组正文不应被折叠（会丢结构）")
		}
	})
}

// TestMergeAdjacentToolCallsEmptyArrayContent 有客户端把「没有正文」发成
// content:[] 而不是 null。若不把空数组算空，背靠背的两条 assistant(tool_calls)
// 就不合并 → 上游 deepseek 系照样判 11148（正是本函数要挡的形态，条件漏了空数组）。
func TestMergeAdjacentToolCallsEmptyArrayContent(t *testing.T) {
	first := tcMsg("c00")
	first["content"] = []any{}
	second := tcMsg("c01")
	second["content"] = []any{}
	out, changed := mergeAdjacentToolCalls([]any{first, second})
	if !changed {
		t.Fatal("content 为空数组应按空正文处理并合并")
	}
	if len(out) != 1 {
		t.Fatalf("应合成一条，实际 %d", len(out))
	}
}

// TestMergeAdjacentToolCallsKeepsReasoningContent deepseek 多轮要求 assistant 带
// 思维链回填（thinking.go 的 reasoning_content），合并时丢弃会换一个错误。
// 两边都有则换行拼接。
func TestMergeAdjacentToolCallsKeepsReasoningContent(t *testing.T) {
	first := tcMsg("c00")
	first["reasoning_content"] = "r1"
	second := tcMsg("c01")
	second["reasoning_content"] = "r2"
	out, changed := mergeAdjacentToolCalls([]any{first, second})
	if !changed {
		t.Fatal("期望合并")
	}
	rc, _ := out[0].(map[string]any)["reasoning_content"].(string)
	if rc != "r1\nr2" {
		t.Fatalf("reasoning_content = %q want %q", rc, "r1\nr2")
	}
}

// TestMergeAdjacentThroughPrepareBody 全链路：出站管线必须把「拆开的并行调用」
// 归一到上游认可的形态（对照实验：合成一条 → 200；拆成两条 → deepseek 系 503/11148）。
// 同时验证三步顺序 merge → repack → cleanup 不会互相打架。
func TestMergeAdjacentThroughPrepareBody(t *testing.T) {
	body := `{"model":"deepseek-v4.1-flash","messages":[
		{"role":"user","content":"跑两个命令"},
		{"role":"assistant","content":null,"tool_calls":[{"id":"call_00_a","type":"function","function":{"name":"exec_command","arguments":"{\"cmd\":\"pwd\"}"}}]},
		{"role":"assistant","content":null,"tool_calls":[{"id":"call_01_b","type":"function","function":{"name":"exec_command","arguments":"{\"cmd\":\"ls\"}"}}]},
		{"role":"tool","tool_call_id":"call_00_a","content":"/opt"},
		{"role":"tool","tool_call_id":"call_01_b","content":"a b c"}
	]}`
	out := PrepareBodyOptWithEfforts([]byte(body), false, nil)
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("unmarshal 出站 body: %v", err)
	}
	msgs := obj["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("拆开的两条 assistant 应合成一条（5 → 4），实际 %d：%#v", len(msgs), msgs)
	}
	if got := callIDsOf(msgs[1]); !reflect.DeepEqual(got, []string{"call_00_a", "call_01_b"}) {
		t.Fatalf("合并后 tool_calls = %v want [call_00_a call_01_b]", got)
	}
	assertPairingSymmetric(t, msgs)
}

// ---------------------------------------------------------------------------
// 空 function.name
// ---------------------------------------------------------------------------

// TestCleanupOrphanEmptyFunctionName 线上 11133：上游流式响应在工具调用 name 分片
// 到达前异常结束（200 空流也算）时，聚合器产出 name 为空的占位条目，客户端把它写进
// 会话历史。下一轮原样转发，上游判 400 code=11133（tool_calls[i].function.name must
// be a non-empty string）——**带不带配对结果都会拒**，整条会话从此每轮 503。
// 修复：按 id 对称裁剪时否决空 name 条目（调用与其结果成对剔除）。
func TestCleanupOrphanEmptyFunctionName(t *testing.T) {
	emptyName := func(id string) map[string]any {
		return map[string]any{"id": id, "type": "function", "function": map[string]any{"name": "", "arguments": "{}"}}
	}
	messages := []any{
		map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{
			emptyName("bad"), // 脏条目：name 为空串
			map[string]any{"id": "good", "type": "function", "function": map[string]any{"name": "read", "arguments": "{}"}},
		}},
		map[string]any{"role": "tool", "tool_call_id": "bad", "content": "r0"},
		map[string]any{"role": "tool", "tool_call_id": "good", "content": "r1"},
	}
	out, changed := cleanupOrphanToolCalls(messages)
	if !changed {
		t.Fatal("空 name 条目应被剔除")
	}
	asst := out[0].(map[string]any)
	tcs := asst["tool_calls"].([]any)
	if len(tcs) != 1 {
		t.Fatalf("只应保留 good，实际 %#v", tcs)
	}
	if id, _ := tcs[0].(map[string]any)["id"].(string); id != "good" {
		t.Fatalf("保留的应是 good，实际 %s", id)
	}
	// 带结果的脏条目其**结果也必须成对剔除**，否则残留孤儿 tool 结果同样判 11148。
	assertPairingSymmetric(t, out)
}

// TestCleanupOrphanEmptyNameWhitespaceOnly 纯空白 name 同样判脏——上游的
// non-empty 校验不认只含空白的串。
func TestCleanupOrphanEmptyNameWhitespaceOnly(t *testing.T) {
	messages := []any{
		map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{
			map[string]any{"id": "ws", "type": "function", "function": map[string]any{"name": " \t ", "arguments": "{}"}},
		}},
		map[string]any{"role": "tool", "tool_call_id": "ws", "content": "r"},
	}
	out, changed := cleanupOrphanToolCalls(messages)
	if !changed {
		t.Fatal("纯空白 name 应判脏")
	}
	if _, ok := out[0].(map[string]any)["tool_calls"]; ok {
		t.Fatalf("整批脏条目被剔后应删除 tool_calls 键：%#v", out[0])
	}
}

// TestCleanupOrphanEmptyNameMissingFieldsNotDirty 判定精确收窄为「function 对象存在、
// name 字段存在且为空白串」：缺 function / 缺 name 字段的占位条目不判脏（上游不因此
// 拒参），与既有「空 id 双侧不识别」用例同口径，不扩大打击面。
func TestCleanupOrphanEmptyNameMissingFieldsNotDirty(t *testing.T) {
	cases := []struct {
		name string
		tc   map[string]any
	}{
		{"缺 function 字段", map[string]any{"id": "x", "type": "function"}},
		{"缺 name 字段", map[string]any{"id": "x", "type": "function", "function": map[string]any{"arguments": "{}"}}},
		{"name 非字符串", map[string]any{"id": "x", "type": "function", "function": map[string]any{"name": 42}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			messages := []any{
				map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{c.tc}},
				map[string]any{"role": "tool", "tool_call_id": "x", "content": "r"},
			}
			out, changed := cleanupOrphanToolCalls(messages)
			if changed {
				t.Fatalf("占位条目不判脏，不应改动：%#v", out)
			}
		})
	}
}

// TestCleanupOrphanEmptyNameWithoutResult 空 name 且**无结果**的条目仍走既有孤儿
// 路径（收集阶段照常登记 callIDs，保住 hasTraffic 语义——否则这种本可被孤儿规则
// 删掉的形态会漏出站）。
func TestCleanupOrphanEmptyNameWithoutResult(t *testing.T) {
	messages := []any{
		map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{
			map[string]any{"id": "bad", "type": "function", "function": map[string]any{"name": ""}},
		}},
		map[string]any{"role": "user", "content": "next"},
	}
	out, changed := cleanupOrphanToolCalls(messages)
	if !changed {
		t.Fatal("空 name 且无结果的条目应被剔除")
	}
	if _, ok := out[0].(map[string]any)["tool_calls"]; ok {
		t.Fatalf("应删除 tool_calls 键：%#v", out[0])
	}
}
