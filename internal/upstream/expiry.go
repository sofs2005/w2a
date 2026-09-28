// 积分包真实到期解析：CycleEndTime 优先、DeductionEndTime 回退，产出有序到期批次。
package upstream

import (
	"sort"
	"time"
)

// CreditExpiry 单个积分包的到期批次（真到期时刻 + 该包剩余可用积分）。
//
// 「真到期」的口径与面板（panel/internal/upstream/client.go 的 expiryString）一致：
// CycleEndTime 优先，缺失/解析失败时回退 DeductionEndTime。两者的取舍是实测出来的：
// 按周期发量的包（「CodeBuddy个人体验版」）CycleEndTime 是本周期边界（月末 23:59:59），
// 而 DeductionEndTime 是 2034 年——与注册日同月日、恰隔 10 年，是**账户级的登记上限**，
// 不是这批积分的作废时刻。实证：上游扣费**先扣该包**（余额在动），而更晚到期的包分文未动。
// 取 DeductionEndTime 会把这个包算成 2034 年才到期，永远不进 72 小时紧急窗口，
// 于是每期赠送的积分白白作废——正是本机制要防的事。
type CreditExpiry struct {
	// ExpiresAt 该批积分的真实到期时刻。零值 = 上游两个字段都缺（无到期），
	// 调用方按「永不过期」处理（不参与临期优先）。
	ExpiresAt time.Time
	// Remain 该包当前剩余可用积分（> 0，已按 packageRemainUsed 口径钳过 range）。
	Remain int64
}

// packageExpiryTime 解析单个套餐的真实到期时刻（CycleEndTime 优先，DeductionEndTime 回退）。
// ok=false 表示上游两个字段都没给（无到期）或两者都解析失败——调用方按「无到期」处理，
// 不编造时刻（与面板「不编造」口径一致）。
// 参数按判据优先级排列：周期串在前，毫秒时间戳在后。
func packageExpiryTime(cycleEndTime string, deductionEndMs int64, loc *time.Location) (time.Time, bool) {
	if cycleEndTime != "" {
		if t, err := time.ParseInLocation(packageEndLayout, cycleEndTime, loc); err == nil {
			return t, true
		}
		// 周期串是脏数据：不因此判「无到期」，继续试 DeductionEndTime（少一层信息好过没有）。
	}
	if deductionEndMs > 0 {
		return time.UnixMilli(deductionEndMs), true
	}
	return time.Time{}, false
}

// expiryBatches 从 accounts 产出选号所需的到期信息：
//
//   - batches：**未过期**（到期时刻在 now 之后）且剩余 > 0 的包，按到期时刻升序。
//     无到期（两字段都缺）与已过期的包不产批次——前者永远不会临期，后者的配额上游
//     不会再扣，参与临期优先只会让选号空烧。
//   - total：全部剩余 > 0 的包合计（= 历史 remain 口径，含已过期包）。
//   - inWindow：未过期且在 soon 窗口内（<= now+soon）的剩余合计。
//
// stable 由调用方算 total-inWindow，使 CreditBuckets.Total() 恒等于 remain
// （既有不变量，见 TestUserResourceDetailedSplitsExpiring 的 Total()==remain 断言）。
// 历史 Expiring 口径用 !end.After(now+soon)（**含已过期包**），本函数收窄为「未过期
// 且在窗口内」：已过期积分的权重加成是虚的（那部分余额上游不会再扣，签到刷新即消失）。
func expiryBatches(accounts []resourceAccount, now time.Time, soon time.Duration, loc *time.Location) (batches []CreditExpiry, total, inWindow int64) {
	deadline := now.Add(soon)
	for _, acct := range accounts {
		r := acct.remainUsed()
		if r <= 0 {
			continue
		}
		total += r
		end, ok := packageExpiryTime(acct.CycleEndTime, acct.DeductionEndTime, loc)
		if !ok || !end.After(now) {
			continue // 无到期 / 已过期：不产批次，也不进窗口
		}
		batches = append(batches, CreditExpiry{ExpiresAt: end, Remain: r})
		if soon > 0 && !end.After(deadline) {
			inWindow += r
		}
	}
	sort.SliceStable(batches, func(i, j int) bool { return batches[i].ExpiresAt.Before(batches[j].ExpiresAt) })
	return batches, total, inWindow
}

// resourceAccount 单个套餐的容量字段（userResourceResp 的 Accounts 元素）。
// 具名类型便于 expiryBatches 独立持有切片（匿名 struct 无法在函数签名里复用）。
type resourceAccount struct {
	PackageName      string `json:"PackageName"`
	CycleEndTime     string `json:"CycleEndTime"`     // "2006-01-02 15:04:05"，本周期边界 = 真到期（优先判据）
	DeductionEndTime int64  `json:"DeductionEndTime"` // 毫秒时间戳，周期串缺失/脏数据时的回退
	CapacitySize     int64  `json:"CapacitySize"`
	CapacityRemain   int64  `json:"CapacityRemain"`
	CapacityUsed     int64  `json:"CapacityUsed"`
	// CycleCapacity* 周期版容量字段（有值时优先，见 packageRemainUsed）。
	CycleCapacitySize   int64 `json:"CycleCapacitySize"`
	CycleCapacityRemain int64 `json:"CycleCapacityRemain"`
	CycleCapacityUsed   int64 `json:"CycleCapacityUsed"`
}

// remainUsed 单套餐的 remain（复用 packageRemainUsed，与 ResourceSummary/cmd/credit 同一事实来源）。
func (a resourceAccount) remainUsed() int64 {
	r, _, _ := packageRemainUsed(respAccount{
		CapacityRemain:      a.CapacityRemain,
		CapacityUsed:        a.CapacityUsed,
		CapacitySize:        a.CapacitySize,
		CycleCapacityRemain: a.CycleCapacityRemain,
		CycleCapacityUsed:   a.CycleCapacityUsed,
		CycleCapacitySize:   a.CycleCapacitySize,
	})
	if r < 0 {
		return 0
	}
	return r
}
