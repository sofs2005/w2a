package api

import (
	"encoding/json"
	"testing"

	"workbuddy2api-gui/internal/gateway"
)

// TestModelRealmsPassthrough 钉住 `realms` 字段在面板侧的**双向**存活。
//
// 为什么值得单独一条测试：`gateway.Model` 是白名单式结构体，encoding/json 对未声明的
// 字段**静默丢弃**——网关下发的 `realms` 就这么消失过一次，且没有任何报错、没有任何
// 日志，症状只是「模型与倍率」页的域分栏全落到国内版（前端还有按 id 前缀推断的回退，
// 于是连"字段没了"都看不出来）。这条测试让「漏声明字段」变成编译期之后的立即失败。
//
// 覆盖两段真实链路：
//  1. 解码 —— 网关 /v1/models 的 JSON → gateway.Model（Client.Models 的做法）；
//  2. 编码 —— modelView 序列化回 JSON（/api/models 响应，前端据此分栏）。
func TestModelRealmsPassthrough(t *testing.T) {
	// 与网关 modelList 输出同形：裸名 + realms 标注可用域。
	raw := []byte(`{"data":[
		{"id":"glm-5.2","object":"model","created":1,"owned_by":"workbuddy",
		 "context_length":1000000,"realms":["cn","global"]},
		{"id":"hunyuan-only-cn","object":"model","created":1,"owned_by":"workbuddy",
		 "context_length":128000,"realms":["cn"]}
	]}`)

	var decoded struct {
		Data []gateway.Model `json:"data"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("解码 /v1/models 失败: %v", err)
	}
	if len(decoded.Data) != 2 {
		t.Fatalf("条目数=%d want 2", len(decoded.Data))
	}
	if got := decoded.Data[0].Realms; len(got) != 2 || got[0] != "cn" || got[1] != "global" {
		t.Fatalf("realms 在解码时被丢弃: %v（gateway.Model 漏声明 json tag？）", got)
	}
	if got := decoded.Data[1].Realms; len(got) != 1 || got[0] != "cn" {
		t.Fatalf("单域条目 realms 错误: %v", got)
	}

	// 老网关不下发 realms → nil，且不得在响应里编造一个空数组
	// （前端靠「字段缺席」判断无法按域区分，空数组会被误读为"哪个域都不支持"）。
	var legacy struct {
		Data []gateway.Model `json:"data"`
	}
	if err := json.Unmarshal([]byte(`{"data":[{"id":"x","object":"model"}]}`), &legacy); err != nil {
		t.Fatal(err)
	}
	if legacy.Data[0].Realms != nil {
		t.Errorf("缺省 realms 应为 nil，得到 %v", legacy.Data[0].Realms)
	}
	blob, err := json.Marshal(modelView{Model: legacy.Data[0]})
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	if err := json.Unmarshal(blob, &back); err != nil {
		t.Fatal(err)
	}
	if _, present := back["realms"]; present {
		t.Errorf("缺省 realms 不应出现在响应里（前端据此判断老网关）: %s", blob)
	}

	// 重新编码后 realms 必须原样透出，否则前端拿不到。
	blob, err = json.Marshal(modelView{Model: decoded.Data[0]})
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(blob, &back); err != nil {
		t.Fatal(err)
	}
	rs, ok := back["realms"].([]any)
	if !ok || len(rs) != 2 {
		t.Fatalf("重新编码后 realms 丢失或形态错误: %s", blob)
	}
	if rs[0] != "cn" || rs[1] != "global" {
		t.Errorf("realms 顺序/内容被改动: %v", rs)
	}
}
