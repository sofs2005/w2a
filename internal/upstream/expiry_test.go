package upstream

import (
	"net/http"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// TestExpiryPrefersCycleEndTime 到期判据是 CycleEndTime（本周期边界，积分到期即作废），
// 优先于 DeductionEndTime——后者对按周期发量的包只是账户级登记上限（见 expiry.go）。
// 场景取自实测：「个人体验版」CycleEndTime=本周期边界，DeductionEndTime=2034 年。
func TestExpiryPrefersCycleEndTime(t *testing.T) {
	now := time.Now()
	cycle := now.Add(3 * 24 * time.Hour).Format(packageEndLayout) // 真到期：3 天后（本周期边界）
	deduction := now.Add(10 * 365 * 24 * time.Hour).UnixMilli()   // 2034 年那类账户级上限
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return mkDetailedResp(
			`{"PackageName":"个人体验版","CycleEndTime":"` + cycle + `","DeductionEndTime":` + itoa(deduction) + `,"CycleCapacitySize":500,"CycleCapacityRemain":500,"CycleCapacityUsed":0}`), nil
	})
	// 7 天窗口：正确口径（CycleEndTime）应归 Expiring；误用 DeductionEndTime 会归 Stable，
	// 该包就永远不进紧急窗口，每期赠送的积分白白作废。
	remain, buckets, expiries, err := c.UserResourceExpiry(&auth.Auth{AccessToken: "at"}, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("expiry: %v", err)
	}
	if remain != 500 || buckets.Expiring != 500 || buckets.Stable != 0 {
		t.Errorf("remain=%d buckets=%+v, want 500/{Expiring:500 Stable:0}（按周期边界判定）", remain, buckets)
	}
	if len(expiries) != 1 {
		t.Fatalf("批次 = %d 条, want 1", len(expiries))
	}
	if got := expiries[0].ExpiresAt.Format(packageEndLayout); got != cycle {
		t.Errorf("批次到期 = %q, want %q（CycleEndTime）", got, cycle)
	}
}

// TestExpiryFallsBackToDeductionEndTime 缺 CycleEndTime 时回退 DeductionEndTime。
func TestExpiryFallsBackToDeductionEndTime(t *testing.T) {
	now := time.Now()
	deduction := now.Add(3 * 24 * time.Hour)
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return mkDetailedResp(
			`{"PackageName":"奖励包","DeductionEndTime":` + itoa(deduction.UnixMilli()) + `,"CycleCapacitySize":1500,"CycleCapacityRemain":1200,"CycleCapacityUsed":300}`), nil
	})
	_, buckets, expiries, err := c.UserResourceExpiry(&auth.Auth{AccessToken: "at"}, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("expiry: %v", err)
	}
	if buckets.Expiring != 1200 {
		t.Errorf("expiring=%d want 1200（回退 DeductionEndTime 且落在 7 天窗内）", buckets.Expiring)
	}
	if len(expiries) != 1 || expiries[0].Remain != 1200 {
		t.Fatalf("批次 = %+v, want 单条 Remain=1200", expiries)
	}
	if got := expiries[0].ExpiresAt.UnixMilli(); got != deduction.UnixMilli() {
		t.Errorf("批次到期 = %d, want %d（DeductionEndTime）", got, deduction.UnixMilli())
	}
}

// TestExpiryBatchesSortedAscending 多包批次按到期时刻升序（选号取首条 = 最早到期）。
func TestExpiryBatchesSortedAscending(t *testing.T) {
	now := time.Now()
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return mkDetailedResp(
			`{"PackageName":"晚","DeductionEndTime":` + itoa(now.Add(240*time.Hour).UnixMilli()) + `,"CycleCapacitySize":100,"CycleCapacityRemain":100,"CycleCapacityUsed":0},` +
				`{"PackageName":"早","DeductionEndTime":` + itoa(now.Add(2*time.Hour).UnixMilli()) + `,"CycleCapacitySize":50,"CycleCapacityRemain":50,"CycleCapacityUsed":0},` +
				`{"PackageName":"中","DeductionEndTime":` + itoa(now.Add(30*time.Hour).UnixMilli()) + `,"CycleCapacitySize":70,"CycleCapacityRemain":70,"CycleCapacityUsed":0}`), nil
	})
	_, _, expiries, err := c.UserResourceExpiry(&auth.Auth{AccessToken: "at"}, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("expiry: %v", err)
	}
	if len(expiries) != 3 {
		t.Fatalf("批次 = %d 条, want 3", len(expiries))
	}
	for i := 1; i < len(expiries); i++ {
		if expiries[i].ExpiresAt.Before(expiries[i-1].ExpiresAt) {
			t.Fatalf("批次未按到期升序：%+v", expiries)
		}
	}
	if expiries[0].Remain != 50 || expiries[2].Remain != 100 {
		t.Errorf("升序后剩余量错位：%+v", expiries)
	}
}

// TestExpiryDropsExpiredAndZeroRemain 已过期包与零余额包不产批次：
// 已过期配额上游不会再扣，参与临期优先只会让选号空烧；零余额包无消耗价值。
func TestExpiryDropsExpiredAndZeroRemain(t *testing.T) {
	now := time.Now()
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return mkDetailedResp(
			`{"PackageName":"已过期","DeductionEndTime":` + itoa(now.Add(-2*time.Hour).UnixMilli()) + `,"CycleCapacitySize":100,"CycleCapacityRemain":100,"CycleCapacityUsed":0},` +
				`{"PackageName":"已用尽","DeductionEndTime":` + itoa(now.Add(2*time.Hour).UnixMilli()) + `,"CycleCapacitySize":100,"CycleCapacityRemain":0,"CycleCapacityUsed":100},` +
				`{"PackageName":"有效","DeductionEndTime":` + itoa(now.Add(5*time.Hour).UnixMilli()) + `,"CycleCapacitySize":40,"CycleCapacityRemain":40,"CycleCapacityUsed":0}`), nil
	})
	remain, buckets, expiries, err := c.UserResourceExpiry(&auth.Auth{AccessToken: "at"}, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("expiry: %v", err)
	}
	if len(expiries) != 1 || expiries[0].Remain != 40 {
		t.Fatalf("批次 = %+v, want 仅剩有效的 40", expiries)
	}
	// 已过期包的余额仍计入总量（上游 remain 口径未变），但**不计入 Expiring**——
	// 那部分积分配额上游不会再扣，标成"快过期"是虚的。
	if remain != 140 {
		t.Errorf("remain=%d want 140（总量口径不变，含已过期包）", remain)
	}
	if buckets.Expiring != 40 {
		t.Errorf("expiring=%d want 40（仅未过期且在窗内）", buckets.Expiring)
	}
	if buckets.Total() != remain {
		t.Errorf("Total()=%d != remain=%d（不变量：两桶合计=总量）", buckets.Total(), remain)
	}
}

// TestExpiryNoWindowAllStable soon<=0 时不分桶（全部归 Stable），但**批次照常产出**
// （72 小时硬优先只依赖批次，与 7 天权重窗口无关）。
func TestExpiryNoWindowAllStable(t *testing.T) {
	now := time.Now()
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return mkDetailedResp(
			`{"PackageName":"p","DeductionEndTime":` + itoa(now.Add(3*time.Hour).UnixMilli()) + `,"CycleCapacitySize":100,"CycleCapacityRemain":80,"CycleCapacityUsed":20}`), nil
	})
	_, buckets, expiries, err := c.UserResourceExpiry(&auth.Auth{AccessToken: "at"}, 0)
	if err != nil {
		t.Fatalf("expiry: %v", err)
	}
	if buckets.Expiring != 0 || buckets.Stable != 80 {
		t.Errorf("buckets=%+v want {Expiring:0 Stable:80} when soon=0", buckets)
	}
	if len(expiries) != 1 {
		t.Errorf("批次应照常产出（硬优先不依赖分桶窗口），got %+v", expiries)
	}
}

// TestExpiryMissingBothFieldsNoBatch 两个字段都缺 → 无到期，不产批次也不进窗口
// （保守归 Stable，不编造到期时刻）。
func TestExpiryMissingBothFieldsNoBatch(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return mkDetailedResp(
			`{"PackageName":"无到期","CycleCapacitySize":100,"CycleCapacityRemain":80,"CycleCapacityUsed":20}`), nil
	})
	remain, buckets, expiries, err := c.UserResourceExpiry(&auth.Auth{AccessToken: "at"}, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("expiry: %v", err)
	}
	if len(expiries) != 0 {
		t.Errorf("无到期包不该产批次，got %+v", expiries)
	}
	if remain != 80 || buckets.Expiring != 0 || buckets.Stable != 80 {
		t.Errorf("remain=%d buckets=%+v, want 80/{0 80}", remain, buckets)
	}
}

// TestExpiryInvalidCycleEndTimeNoBatch 周期串解析失败且无回退字段 → 无到期（不产批次）。
func TestExpiryInvalidCycleEndTimeNoBatch(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return mkDetailedResp(
			`{"PackageName":"脏数据","CycleEndTime":"not-a-time","CycleCapacitySize":100,"CycleCapacityRemain":80,"CycleCapacityUsed":20}`), nil
	})
	_, buckets, expiries, err := c.UserResourceExpiry(&auth.Auth{AccessToken: "at"}, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("expiry: %v", err)
	}
	if len(expiries) != 0 || buckets.Expiring != 0 {
		t.Errorf("解析失败应按无到期处理：expiries=%+v buckets=%+v", expiries, buckets)
	}
}

// TestExpiryDirtyCycleEndTimeFallsBack 周期串是脏数据时继续试 DeductionEndTime——
// 「少一层信息好过没有」：不因周期串坏掉就断言这个包无到期。
func TestExpiryDirtyCycleEndTimeFallsBack(t *testing.T) {
	now := time.Now()
	deduction := now.Add(3 * 24 * time.Hour)
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return mkDetailedResp(
			`{"PackageName":"脏周期","CycleEndTime":"not-a-time","DeductionEndTime":`+itoa(deduction.UnixMilli())+`,"CycleCapacitySize":100,"CycleCapacityRemain":80,"CycleCapacityUsed":20}`), nil
	})
	_, buckets, expiries, err := c.UserResourceExpiry(&auth.Auth{AccessToken: "at"}, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("expiry: %v", err)
	}
	if len(expiries) != 1 || expiries[0].Remain != 80 {
		t.Fatalf("应回退 DeductionEndTime 产批次，got %+v", expiries)
	}
	if got := expiries[0].ExpiresAt.UnixMilli(); got != deduction.UnixMilli() {
		t.Errorf("批次到期 = %d, want %d（回退 DeductionEndTime）", got, deduction.UnixMilli())
	}
	if buckets.Expiring != 80 {
		t.Errorf("expiring=%d want 80（回退值落在 7 天窗内）", buckets.Expiring)
	}
}

// TestUserResourceDetailedUnchanged 向后兼容：UserResourceDetailed 的三元组签名与
// 口径不变（它现在是 UserResourceExpiry 的薄封装），Total()==remain 不变量保持。
func TestUserResourceDetailedUnchanged(t *testing.T) {
	now := time.Now()
	in3d := now.Add(3 * 24 * time.Hour).Format(packageEndLayout)
	in30d := now.Add(30 * 24 * time.Hour).Format(packageEndLayout)
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return mkDetailedResp(
			`{"PackageName":"奖励包","CycleEndTime":"` + in3d + `","CycleCapacitySize":1500,"CycleCapacityRemain":1200,"CycleCapacityUsed":300},` +
				`{"PackageName":"周期包","CycleEndTime":"` + in30d + `","CycleCapacitySize":500,"CycleCapacityRemain":300,"CycleCapacityUsed":200}`), nil
	})
	remain, buckets, err := c.UserResourceDetailed(&auth.Auth{AccessToken: "at"}, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("detailed: %v", err)
	}
	if remain != buckets.Total() {
		t.Errorf("remain=%d != Total()=%d", remain, buckets.Total())
	}
	if remain != 1500 || buckets.Expiring != 1200 || buckets.Stable != 300 {
		t.Errorf("remain=%d buckets=%+v, want 1500/{1200 300}", remain, buckets)
	}
}

// itoa 复用同包 modelsdev_test.go 的实现（毫秒时间戳拼接 JSON 用）。
