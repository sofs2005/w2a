// buddy_box.go growth 域「猫猫盲盒」接口：用能量开盲盒换 Buddy。
//
//	GET  /activity/growth/buddy/quota → {affordable, balance, cost_per_open, max_open_count}
//	POST /activity/growth/buddy/open  → {"count":1}（消耗 cost_per_open 能量）
//
// 逆向来源：CN web 成长中心 SPA 实测抓包（2026-10-27，/profile/growth-center 页面
// 「开启盲盒」按钮，位于「派猫猫旅行」左侧）。
//
// 能量是**消耗性**资源：每开一次扣 cost_per_open（实测 10）。上游另有 total_earned /
// total_consumed 计数（GET /activity/growth/energy），但开箱闸口以 quota.affordable 为准——
// 它是上游自己算的「当前还能开几个」，比本地 balance/10 可靠（可能含额外限制）。
//
// 与 travel.go 同域同信封（growthJSON = chatBase + BillingHeaders）。
package upstream

import (
	"encoding/json"
	"net/http"

	"workbuddy2api/internal/auth"
)

// buddy 盲盒路径（实测）。
const (
	buddyQuotaPath = "/activity/growth/buddy/quota"
	buddyOpenPath  = "/activity/growth/buddy/open"
)

// BuddyQuota 盲盒开箱额度（GET /activity/growth/buddy/quota）。
//
// Affordable 是权威闸：>0 才能开。Balance/CostPerOpen 供日志与本地估算，
// MaxOpenCount 是上游对单次请求 count 的上限（实测 5）——我们逐个开，天然不触顶。
type BuddyQuota struct {
	Affordable   int `json:"affordable"`
	Balance      int `json:"balance"`
	CostPerOpen  int `json:"cost_per_open"`
	MaxOpenCount int `json:"max_open_count"`
}

// BuddyQuotaQuery 查询当前盲盒开箱额度。
func (c *Client) BuddyQuotaQuery(a *auth.Auth) (*BuddyQuota, error) {
	data, err := c.growthJSON(a, http.MethodGet, buddyQuotaPath, nil)
	if err != nil {
		return nil, err
	}
	var q BuddyQuota
	if err := json.Unmarshal(data, &q); err != nil {
		return nil, err
	}
	return &q, nil
}

// BuddyInstance 开出的 Buddy 实例（data.results[].instance）。
// 字段按实测响应保留；后续可能为空的时间字段用 string 承接，避免零值语义歧义。
type BuddyInstance struct {
	ID         int64  `json:"id"`
	TemplateID int    `json:"template_id"`
	IsShiny    int    `json:"is_shiny"`
	SoulDesc   string `json:"soul_desc"`
	Source     string `json:"source"` // "gacha" = 盲盒开出
}

// BuddyTemplate 开出的 Buddy 模板（data.results[].template）：名字与稀有度在此。
type BuddyTemplate struct {
	ID          int    `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Personality string `json:"personality"`
}

// BuddyOpenResult 单个开箱结果（data.results[] 元素）。
type BuddyOpenResult struct {
	Instance BuddyInstance `json:"instance"`
	Template BuddyTemplate `json:"template"`
}

// BuddyOpenResp 开箱响应（POST /activity/growth/buddy/open）。
type BuddyOpenResp struct {
	Count   int               `json:"count"`
	Results []BuddyOpenResult `json:"results"`
}

// BuddyOpen 开盲盒 count 次（count=1 时逐个开，调用方按需循环）。
//
// count 必须 >=1：上游对 count<=0 的行为未实测，这里直接拦掉，避免把「没想开」
// 误发成一次扣能量的请求。count 也不要超过 quota.MaxOpenCount（上游上限）。
func (c *Client) BuddyOpen(a *auth.Auth, count int) (*BuddyOpenResp, error) {
	if count < 1 {
		count = 1
	}
	data, err := c.growthJSON(a, http.MethodPost, buddyOpenPath, map[string]any{"count": count})
	if err != nil {
		return nil, err
	}
	var resp BuddyOpenResp
	if len(data) > 0 {
		// 结果字段缺失不视为失败：能量已扣、盲盒已开，调用方按 0 条结果记日志即可。
		_ = json.Unmarshal(data, &resp)
	}
	return &resp, nil
}
