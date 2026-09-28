package pool

import (
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// setBatches 测试辅助：写余额 + 逐包到期批次（模拟签到刷新的权威覆盖）。
func setBatches(p *Pool, uid string, credits int64, expiring int64, batches ...CreditBatch) {
	p.SetCreditsExpiring(uid, credits, expiring, batches)
}

func inHours(h float64) time.Time { return time.Now().Add(time.Duration(h * float64(time.Hour))) }

// TestUrgentExpiryBeatsStable 核心验收：积分 72 小时内到期的号必须压过积分更多的号
// （窗口内有紧急候选时，最早到期优先于原有成本/权重逻辑）。
func TestUrgentExpiryBeatsStable(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetCostExploreInterval(0)
	p.SetRandomSource(func(n int64) int64 { return 0 })
	p.Add(&auth.Auth{UID: "urgent"})
	p.Add(&auth.Auth{UID: "stable"})
	// stable 积分远高于 urgent：纯权重口径下 stable 必胜，硬优先必须压过它。
	setBatches(p, "urgent", 100, 100, CreditBatch{ExpiresAt: inHours(10), Remain: 100})
	setBatches(p, "stable", 1_000_000, 0, CreditBatch{ExpiresAt: inHours(24 * 60), Remain: 1_000_000})

	for i := 0; i < 20; i++ {
		a := p.PickExcludingForRealm(nil, "m", "")
		if a == nil || a.UID != "urgent" {
			t.Fatalf("第 %d 次选中 %v，want urgent（10 小时后到期应压过长期积分）", i, a)
		}
	}
}

// TestUrgentExpiryEarliestWins 多个紧急候选之间按**最早到期时刻**选择。
func TestUrgentExpiryEarliestWins(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetCostExploreInterval(0)
	p.SetRandomSource(func(n int64) int64 { return 0 })
	p.Add(&auth.Auth{UID: "later"})
	p.Add(&auth.Auth{UID: "sooner"})
	// 两者都在窗口内，但 sooner 更早到期；且 later 积分更高（不得翻盘）。
	setBatches(p, "later", 1_000_000, 1_000_000, CreditBatch{ExpiresAt: inHours(60), Remain: 1_000_000})
	setBatches(p, "sooner", 50, 50, CreditBatch{ExpiresAt: inHours(5), Remain: 50})

	for i := 0; i < 20; i++ {
		a := p.PickExcludingForRealm(nil, "m", "")
		if a == nil || a.UID != "sooner" {
			t.Fatalf("第 %d 次选中 %v，want sooner（5h < 60h，均窗口内）", i, a)
		}
	}
}

// TestNoUrgentKeepsLegacyBehavior 窗口外零回归：没有任何账号在 72 小时内到期时，
// 选号必须回到原有权重逻辑（积分高的号占优），不得因"全池最早到期"而被垄断。
func TestNoUrgentKeepsLegacyBehavior(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetCostExploreInterval(0)
	// 随机源恒取 0 → pickWeighted 必然落在候选首（权重降序后的第一位）。
	// 本测试同时验证两件事：窗口外**不**按到期时间硬选，且聚合权重正常生效
	//（若 rich 的 credits 未参与聚合，总权重塌成候选数 2，取 0 会选中 poor）。
	p.SetRandomSource(func(n int64) int64 { return 0 })
	p.Add(&auth.Auth{UID: "rich"})
	p.Add(&auth.Auth{UID: "poor"})
	// 两个批次都在 72h 窗口外（120h / 90d），但 poor 的到期时刻**更早**——
	// 若紧急分支误判窗口，穷号会被硬选；正确行为是走原权重逻辑选 rich。
	setBatches(p, "rich", 1_000_000, 0, CreditBatch{ExpiresAt: inHours(24 * 90), Remain: 1_000_000})
	setBatches(p, "poor", 10, 0, CreditBatch{ExpiresAt: inHours(24 * 5), Remain: 10})

	for i := 0; i < 20; i++ {
		a := p.PickExcludingForRealm(nil, "m", "")
		if a == nil || a.UID != "rich" {
			t.Fatalf("第 %d 次选中 %v，want rich（窗口外返回原权重逻辑，积分高者胜）", i, a)
		}
	}
}

// TestFreeBeatsUrgentPaid 免费号压过临期非免费号（用户明确规则：
// "不是最低成本层，而是只有免费模型才能排到最早到期前面"）——但仅限于
// **免费号自身也临期**时接管；免费号都不临期时交还原逻辑（见下个测试）。
func TestFreeBeatsUrgentPaid(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetCostExploreInterval(0)
	p.SetRandomSource(func(n int64) int64 { return 0 })
	p.Add(&auth.Auth{UID: "free"})
	p.Add(&auth.Auth{UID: "paid-urgent"})
	p.NoteModelCost("free", "m", 0, 1000)          // tier 0 实测免费
	p.NoteModelCost("paid-urgent", "m", 2.9, 1000) // tier 2 实测收费
	// 免费号也在窗口内（但比 paid 晚到期）→ 免费层优先，不因 paid 更早到期而改变。
	setBatches(p, "free", 100, 100, CreditBatch{ExpiresAt: inHours(60), Remain: 100})
	setBatches(p, "paid-urgent", 100, 100, CreditBatch{ExpiresAt: inHours(2), Remain: 100})

	for i := 0; i < 20; i++ {
		a := p.PickExcludingForRealm(nil, "m", "")
		if a == nil || a.UID != "free" {
			t.Fatalf("第 %d 次选中 %v，want free（免费号不为了烧临期积分去付钱）", i, a)
		}
	}
}

// TestFreeNotUrgentFallsBackToLegacy 有免费号但免费号都不临期时，紧急分支**必须放手**：
// 否则会退化成"永远选字典序最小的免费号"（免费号可能长期无到期批次）——这正是
// 用户担心的"某个账号被垄断"的形态。交还原逻辑后由 Top5 + 加权随机打散。
func TestFreeNotUrgentFallsBackToLegacy(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetCostExploreInterval(0)
	p.Add(&auth.Auth{UID: "free-b"})
	p.Add(&auth.Auth{UID: "free-a"})
	p.NoteModelCost("free-a", "m", 0, 1000)
	p.NoteModelCost("free-b", "m", 0, 1000)
	// 免费号无到期批次（长期有效）；付费号 2 小时后到期。
	setBatches(p, "free-a", 1000, 0)
	setBatches(p, "free-b", 1000, 0)
	p.Add(&auth.Auth{UID: "paid-urgent"})
	p.NoteModelCost("paid-urgent", "m", 2.9, 1000)
	setBatches(p, "paid-urgent", 100, 100, CreditBatch{ExpiresAt: inHours(2), Remain: 100})

	// 免费层优先仍成立（常规成本分层），但必须是在免费号之间**随机/轮换**，
	// 而不是永远 free-a（字典序最小）。
	seen := map[string]int{}
	for i := 0; i < 100; i++ {
		a := p.PickExcludingForRealm(nil, "m", "")
		if a == nil {
			t.Fatal("pick returned nil")
		}
		seen[a.UID]++
	}
	if seen["paid-urgent"] != 0 {
		t.Errorf("免费层优先应仍然生效，paid-urgent 被选中 %d 次", seen["paid-urgent"])
	}
	if seen["free-a"] == 0 || seen["free-b"] == 0 {
		t.Errorf("免费号之间应打散（不得垄断单号）：free-a=%d free-b=%d", seen["free-a"], seen["free-b"])
	}
}

// TestUnknownCostUrgentBeatsCheaperUnknown 未知成本之间不看 tier，看到期时间：
// "不让未知成本层单凭 tier 排在更早到期账号前面"（此时两号同为 tier 1）。
func TestUnknownCostUrgentBeatsCheaperUnknown(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetCostExploreInterval(0)
	p.SetRandomSource(func(n int64) int64 { return 0 })
	p.Add(&auth.Auth{UID: "u-later"})
	p.Add(&auth.Auth{UID: "u-sooner"})
	setBatches(p, "u-later", 1000, 1000, CreditBatch{ExpiresAt: inHours(70), Remain: 1000})
	setBatches(p, "u-sooner", 1000, 1000, CreditBatch{ExpiresAt: inHours(1), Remain: 1000})

	for i := 0; i < 20; i++ {
		a := p.PickExcludingForRealm(nil, "m", "")
		if a == nil || a.UID != "u-sooner" {
			t.Fatalf("第 %d 次选中 %v，want u-sooner（同层之间按到期时间）", i, a)
		}
	}
}

// TestDebitAdvancesEarliestBatch 消耗后最早批次前移：最早批次被扣穿后，账号的最早
// 到期时刻推进到下一条；若剩余批次全部超出窗口，则退出紧急集（不再获得硬优先）。
func TestDebitAdvancesEarliestBatch(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	setBatches(p, "u1", 150, 150,
		CreditBatch{ExpiresAt: inHours(2), Remain: 50}, // 最早：先被扣
		CreditBatch{ExpiresAt: inHours(24 * 30), Remain: 100})

	p.mu.Lock()
	before, ok := p.byUID["u1"].earliestExpiryAt(time.Now())
	p.mu.Unlock()
	if !ok || before.Sub(time.Now()) > 3*time.Hour {
		t.Fatalf("初始最早到期应 ~2h，got %v (ok=%v)", before, ok)
	}

	// 扣 50：第一条耗尽被移除。
	p.NoteModelCost("u1", "m", 50, 1000)
	p.mu.Lock()
	e := p.byUID["u1"]
	if len(e.creditBatches) != 1 {
		t.Fatalf("耗尽批次应被移除，剩余 %d 条", len(e.creditBatches))
	}
	after, ok := e.earliestExpiryAt(time.Now())
	urgent, isUrgent := e.urgentExpiryAt(time.Now())
	p.mu.Unlock()
	if !ok {
		t.Fatal("应仍有未到期批次")
	}
	if after.Sub(time.Now()) < 29*24*time.Hour {
		t.Errorf("最早到期应推进到 30 天批次，got %v", after)
	}
	if isUrgent {
		t.Errorf("批次推进后应退出紧急窗口，urgent at %v", urgent)
	}
}

// TestBatchesPersistRoundTrip 持久化往返：批次落盘/恢复无损，且过期条目被惰性过滤。
func TestBatchesPersistRoundTrip(t *testing.T) {
	dir := t.TempDir()
	fp := dir + "/state.json"
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	setBatches(p, "u1", 300, 300,
		CreditBatch{ExpiresAt: inHours(6), Remain: 100},
		CreditBatch{ExpiresAt: inHours(48), Remain: 200})
	p.Flush()

	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"}) // SyncToDir 换全凭证
	p2.mu.RLock()
	e := p2.byUID["u1"]
	defer p2.mu.RUnlock()
	if len(e.creditBatches) != 2 {
		t.Fatalf("恢复批次 %d 条，want 2", len(e.creditBatches))
	}
	if e.creditBatches[0].Remain != 100 || e.creditBatches[1].Remain != 200 {
		t.Errorf("批次剩余量往返失真：%+v", e.creditBatches)
	}
	if !e.creditBatches[0].ExpiresAt.Before(e.creditBatches[1].ExpiresAt) {
		t.Errorf("恢复后应按到期升序：%+v", e.creditBatches)
	}
}

// TestBatchesExpiredDroppedOnLoad 恢复时剔除已过期批次（上游不会再扣，留着会虚报临期）。
func TestBatchesExpiredDroppedOnLoad(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.mu.Lock()
	p.byUID["u1"].credits = 300
	p.byUID["u1"].creditBatches = normalizeBatches([]CreditBatch{
		{ExpiresAt: inHours(-1), Remain: 100}, // 已过期
		{ExpiresAt: inHours(6), Remain: 200},
	}, 300, time.Now())
	n := len(p.byUID["u1"].creditBatches)
	p.mu.Unlock()
	if n != 1 {
		t.Errorf("已过期批次应被剔除，剩 %d 条", n)
	}
}

// TestBatchesClampedToCredits 批次累计不得超 credits（明细是总量的分解，
// 上游脏数据/本地递减 bug 都不能让明细反过来放大决策）。
func TestBatchesClampedToCredits(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	setBatches(p, "u1", 100, 100,
		CreditBatch{ExpiresAt: inHours(2), Remain: 80},
		CreditBatch{ExpiresAt: inHours(48), Remain: 80}) // 累计 160 > credits 100
	p.mu.RLock()
	defer p.mu.RUnlock()
	var sum int64
	for _, b := range p.byUID["u1"].creditBatches {
		sum += b.Remain
	}
	if sum > 100 {
		t.Errorf("批次累计 %d 超过 credits 100", sum)
	}
}

// TestBetterExpiryPickOrdering 直接锁定三级排序语义（避免依赖选号分布间接验证）。
func TestBetterExpiryPickOrdering(t *testing.T) {
	now := time.Now()
	soon := now.Add(time.Hour)
	later := now.Add(48 * time.Hour)

	cases := []struct {
		name         string
		aOK, bOK     bool
		aAt, bAt     time.Time
		aTier, bTier int
		aCost, bCost float64
		aUID, bUID   string
		want         bool
	}{
		{"有批次优于无批次", true, false, soon, time.Time{}, 1, 0, 0, 0, "a", "b", true},
		{"无批次劣于有批次", false, true, time.Time{}, soon, 0, 1, 0, 0, "a", "b", false},
		{"更早到期胜出", true, true, soon, later, 1, 1, 0, 0, "a", "b", true},
		{"更晚到期落败", true, true, later, soon, 1, 1, 0, 0, "a", "b", false},
		{"同到期比成本层", true, true, soon, soon, 0, 2, 0, 3, "a", "b", true},
		{"同到期同层比单价", true, true, soon, soon, 2, 2, 1, 3, "a", "b", true},
		{"全同按 UID 字典序", true, true, soon, soon, 2, 2, 1, 1, "a", "b", true},
		{"全同 UID 靠后落败", true, true, soon, soon, 2, 2, 1, 1, "z", "b", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := betterExpiryPick(c.aAt, c.aOK, c.aTier, c.aCost, c.aUID,
				c.bAt, c.bOK, c.bTier, c.bCost, c.bUID)
			if got != c.want {
				t.Errorf("betterExpiryPick = %v, want %v", got, c.want)
			}
		})
	}
}

// TestUrgentUIDsForModelRealm 粘性初次分配侧：有紧急候选时返回子集（免费层优先），
// 无紧急候选时返回 nil（调用方保持原有哈希分配）。
func TestUrgentUIDsForModelRealm(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "stable"})
	p.Add(&auth.Auth{UID: "urgent"})
	setBatches(p, "stable", 1000, 0, CreditBatch{ExpiresAt: inHours(24 * 60), Remain: 1000})
	setBatches(p, "urgent", 100, 100, CreditBatch{ExpiresAt: inHours(3), Remain: 100})

	uids := p.UrgentUIDsForModelRealm("m", "")
	if len(uids) != 1 || uids[0] != "urgent" {
		t.Fatalf("urgent uids = %v, want [urgent]", uids)
	}

	// 无任何紧急候选 → nil（不是空切片：调用方按"无该维度"处理）。
	p2 := New("")
	p2.Add(&auth.Auth{UID: "s1"})
	setBatches(p2, "s1", 100, 0, CreditBatch{ExpiresAt: inHours(24 * 60), Remain: 100})
	if got := p2.UrgentUIDsForModelRealm("m", ""); got != nil {
		t.Errorf("无紧急候选应返回 nil，got %v", got)
	}
}
