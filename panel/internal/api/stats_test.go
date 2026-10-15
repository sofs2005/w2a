package api

import (
	"encoding/json"
	"strings"
	"testing"

	"workbuddy2api-gui/internal/gateway"
)

// TestStatsOutModelsPassthrough 钉住 `out_models` 在面板侧的**双向**存活。
//
// 为什么值得单独一条测试（与 TestModelRealmsPassthrough / TestAccountStatusLedgerPassthrough
// 同因）：`gateway.RealmStat` 是白名单式结构体，encoding/json 对未声明字段**静默丢弃**
// ——不报错、不告警，症状只是统计页「出站模型」那一列永远显示「同名」（也就是"没走别名"），
// 而这恰好是本次新增这一列要回答的问题本身：**别名到底有没有生效**。
// `realms` / `credits` 在本项目已经各这么消失过一次。
//
// 覆盖两段真实链路：
//  1. 解码 —— 网关 /v1/stats 的 JSON → gateway.Stats（Client.StatsRange 的做法）；
//  2. 编码 —— handleStats 原样透出（`writeJSON(..., map[string]any{"stats": st})`）。
func TestStatsOutModelsPassthrough(t *testing.T) {
	// 与网关 MetricsSnapshotOf 输出同形：裸名一行 + 分域明细，cn 侧发生了别名改写
	// （hy4-preview → hy4-preview-f），global 侧没有（出站名 == 裸名 → 网关不下发该键）。
	raw := []byte(`{"enabled":true,"models":[{
		"model":"hy4-preview","bare":"hy4-preview","requests":2,
		"realms":[
			{"realm":"cn","requests":1,"out_models":["hy4-preview-f"]},
			{"realm":"global","requests":1}
		]
	}]}`)

	var decoded gateway.Stats
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("解码 /v1/stats 失败: %v", err)
	}
	if len(decoded.Models) != 1 || len(decoded.Models[0].Realms) != 2 {
		t.Fatalf("条目结构错误: %+v", decoded.Models)
	}
	cn, gl := decoded.Models[0].Realms[0], decoded.Models[0].Realms[1]

	if got := cn.OutModels; len(got) != 1 || got[0] != "hy4-preview-f" {
		t.Fatalf("out_models 在解码时被丢弃: %v（gateway.RealmStat 漏声明 json tag？）", got)
	}
	// 缺席 ≠ 空数组：网关只在「出站名 ≠ 裸名」时下发，缺席的含义是"这次没走别名改写"。
	// 若解码把它变成非 nil 空切片，前端就无法把「同名」与「网关没上报」区分开。
	if gl.OutModels != nil {
		t.Errorf("缺省 out_models 应为 nil（= 未发生改写），得到 %v", gl.OutModels)
	}

	// 重新编码（handleStats 就是原样透出）：字段必须原样到达前端。
	blob, err := json.Marshal(map[string]any{"stats": decoded})
	if err != nil {
		t.Fatal(err)
	}
	var back struct {
		Stats struct {
			Models []struct {
				Realms []struct {
					Realm     string   `json:"realm"`
					OutModels []string `json:"out_models"`
				} `json:"realms"`
			} `json:"models"`
		} `json:"stats"`
	}
	if err := json.Unmarshal(blob, &back); err != nil {
		t.Fatal(err)
	}
	if len(back.Stats.Models) != 1 || len(back.Stats.Models[0].Realms) != 2 {
		t.Fatalf("重新编码后结构丢失: %s", blob)
	}
	if got := back.Stats.Models[0].Realms[0].OutModels; len(got) != 1 || got[0] != "hy4-preview-f" {
		t.Errorf("重新编码后 out_models 丢失: %s", blob)
	}
	// 未改写的域不得凭空多出 out_models 键（omitempty 生效）。
	if strings.Contains(string(blob), `"realm":"global","out_models"`) {
		t.Errorf("未改写域的 out_models 不应出现在响应里: %s", blob)
	}
}
