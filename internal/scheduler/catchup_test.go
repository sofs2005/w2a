package scheduler

import (
	"context"
	"testing"
	"time"
)

// catchupScheduler 固定随机偏移为 0，让 scheduledSlot 落在整点，便于断言。
// 只留签到 + 旅行两类，其余关掉（补跑扫描会遍历七类，多余时点会干扰计数）。
func catchupScheduler(t *testing.T) *Scheduler {
	t.Helper()
	s := New(Config{
		CheckinHours:       []int{9, 21},
		TravelHours:        []int{9, 21},
		RandomDelayMinutes: 0,
		ActivityDisabled:   true,
		KeepaliveDisabled:  true,
		CatDisabled:        true,
		GrowthDisabled:     true,
		LimitedDisabled:    true,
	})
	return s
}

func kindsOf(ms []missedSlot) []taskKind {
	out := make([]taskKind, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.kind)
	}
	return out
}

// TestMissedSlotsCatchesSlotCrossedByBatch 复现核心 bug：先跑的那类任务批次
// 跑过了头，把同一小时窗口里后一个槽位整个跨过去。
//
// 真实配置下签到与旅行都配在 9/21 点，但 kind 不同 → 随机偏移不同（scheduledSlot
// 把 kind 计入哈希）。签到批次要跑 22~60 分钟，远长于 26 分钟的偏移窗口，于是
// 「后触发的那一类」当天必然丢失。
func TestMissedSlotsCatchesSlotCrossedByBatch(t *testing.T) {
	s := New(Config{
		CheckinHours:       []int{9},
		TravelHours:        []int{9},
		RandomDelayMinutes: 26,
		RandomDelaySeed:    "catchup-test-deployment",
		ActivityDisabled:   true,
		KeepaliveDisabled:  true,
		CatDisabled:        true,
		GrowthDisabled:     true,
		LimitedDisabled:    true,
	})
	day := time.Date(2026, 10, 27, 0, 0, 0, 0, cstZone)

	at1 := s.scheduledSlot(day, 9, taskCheckin)
	at2 := s.scheduledSlot(day, 9, taskTravel)
	if at1.Equal(at2) {
		t.Fatal("前提不成立：两类任务的偏移撞车了，换一个 seed 或日期")
	}
	earlier, later := at1, at2
	earlierKind, laterKind := taskCheckin, taskTravel
	if at2.Before(at1) {
		earlier, later = at2, at1
		earlierKind, laterKind = taskTravel, taskCheckin
	}

	// 先跑的那类唤醒并一直跑到 later 之后（模拟长批次）。
	got := s.missedSlots(earlier, later.Add(10*time.Minute))

	if len(got) != 1 {
		t.Fatalf("missed=%v want exactly [%v@%s]", kindsOf(got), laterKind, later.Format("15:04"))
	}
	if got[0].kind != laterKind {
		t.Errorf("kind=%v want %v", got[0].kind, laterKind)
	}
	if !got[0].at.Equal(later) {
		t.Errorf("at=%v want %v", got[0].at, later)
	}
	// 先跑的那类不该被重复补跑。
	if got[0].kind == earlierKind {
		t.Errorf("补跑了已执行过的 %v", earlierKind)
	}
}

// TestMissedSlotsEmptyWhenBatchWasFast 批次没跨过任何槽位时不补跑（避免无谓重放）。
func TestMissedSlotsEmptyWhenBatchWasFast(t *testing.T) {
	s := catchupScheduler(t)
	day := time.Date(2026, 10, 27, 0, 0, 0, 0, cstZone)

	// 09:00 唤醒，09:03 跑完：下一槽位 21:00 还没到，没有漏跑。
	got := s.missedSlots(day.Add(9*time.Hour), day.Add(9*time.Hour+3*time.Minute))
	if len(got) != 0 {
		t.Fatalf("missed=%v want none", kindsOf(got))
	}
}

// TestMissedSlotsIgnoresSlotsBeforeRanAt 停机期间的槽位不补跑。
//
// 进程停机 3 天后启动，ranAt 是启动后第一次唤醒时刻——更早的槽位属于停机窗口，
// 不该在启动瞬间把几天的历史任务全部重放一遍（那些任务本身按自然日幂等，重放无益
// 且会瞬间打满上游）。
func TestMissedSlotsIgnoresSlotsBeforeRanAt(t *testing.T) {
	s := New(Config{
		CheckinHours:       []int{9},
		TravelHours:        []int{9},
		RandomDelayMinutes: 26,
		RandomDelaySeed:    "catchup-test-deployment",
		ActivityDisabled:   true,
		KeepaliveDisabled:  true,
		CatDisabled:        true,
		GrowthDisabled:     true,
		LimitedDisabled:    true,
	})
	day := time.Date(2026, 10, 27, 0, 0, 0, 0, cstZone)

	at1 := s.scheduledSlot(day, 9, taskCheckin)
	at2 := s.scheduledSlot(day, 9, taskTravel)
	earlier, later := at1, at2
	if at2.Before(at1) {
		earlier, later = at2, at1
	}

	// 10-24 停机，10-27 才在 earlier 时刻唤醒：10-24~10-26 的槽位都在 ranAt 之前，
	// 只有「earlier 之后、later 之前」这一个槽位算漏跑。
	got := s.missedSlots(earlier, later.Add(10*time.Minute))
	for _, m := range got {
		if m.at.Before(earlier) {
			t.Errorf("missed %s@%s 早于 ranAt %s，不该补跑", m.kind, m.at, earlier)
		}
	}
	if len(got) != 1 {
		t.Fatalf("missed=%v want exactly 1 slot", kindsOf(got))
	}
}

// TestMissedSlotsMergesSameInstant 同一时刻的多类任务合并成一个补跑批次，
// 与 nextWake「同刻多类一并执行」口径一致。
func TestMissedSlotsMergesSameInstant(t *testing.T) {
	s := New(Config{
		CheckinHours:       []int{9},
		TravelHours:        []int{9},
		RandomDelayMinutes: 0,
		ActivityDisabled:   true,
		KeepaliveDisabled:  true,
		CatDisabled:        true,
		GrowthDisabled:     true,
		LimitedDisabled:    true,
	})
	day := time.Date(2026, 10, 27, 0, 0, 0, 0, cstZone)

	// 08:00 唤醒，10:00 才跑完：09:00 的签到与旅行都没跑。
	got := s.missedSlots(day.Add(8*time.Hour), day.Add(10*time.Hour))
	if len(got) != 2 {
		t.Fatalf("missed=%v want checkin+travel@09:00", kindsOf(got))
	}
	if got[0].at != got[1].at {
		t.Errorf("同刻槽位应合并，实际 at=%s / %s", got[0].at, got[1].at)
	}
}

// TestMissedSlotsSortedByTime 多个不同时刻的漏跑槽位必须按时间升序补跑，
// 否则「09:00 的签到」可能在「12:00 的限时活动」之后才执行。
func TestMissedSlotsSortedByTime(t *testing.T) {
	s := New(Config{
		CheckinHours:       []int{9},
		TravelHours:        []int{12},
		ActivityHours:      []int{15},
		RandomDelayMinutes: 0,
		KeepaliveDisabled:  true,
		CatDisabled:        true,
		GrowthDisabled:     true,
		LimitedDisabled:    true,
	})
	day := time.Date(2026, 10, 27, 0, 0, 0, 0, cstZone)

	got := s.missedSlots(day.Add(8*time.Hour), day.Add(20*time.Hour))
	if len(got) != 3 {
		t.Fatalf("missed=%v want 3 slots", kindsOf(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i].at.Before(got[i-1].at) {
			t.Fatalf("未按时间升序: %v", got)
		}
	}
	if got[0].kind != taskCheckin || got[1].kind != taskTravel || got[2].kind != taskActivity {
		t.Errorf("order=%v want [checkin travel activity]", kindsOf(got))
	}
}

// TestMissedSlotsSkipsDisabled 已禁用的任务不参与补跑（与 nextWake 同口径）。
func TestMissedSlotsSkipsDisabled(t *testing.T) {
	s := catchupScheduler(t)
	s.mutate(func(c *Config) { c.TravelDisabled = true })
	day := time.Date(2026, 10, 27, 0, 0, 0, 0, cstZone)

	got := s.missedSlots(day.Add(9*time.Hour), day.Add(9*time.Hour+40*time.Minute))
	if len(got) != 0 {
		t.Fatalf("missed=%v want none（旅行已禁用）", kindsOf(got))
	}
}

// TestCatchUpMissedDispatchesTravel 端到端：被跨过的旅行槽位真的被派发了。
func TestCatchUpMissedDispatchesTravel(t *testing.T) {
	fastTravel(t)
	stub := &travelStub{buddy: `{"id":1,"name":"喵"}`, state: `{"state":"idle"}`}
	srv := stub.server()
	defer srv.Close()

	s, _ := newTravelScheduler(t, srv, "u1")
	day := time.Date(2026, 10, 27, 0, 0, 0, 0, cstZone)

	// 只让旅行成为漏跑槽位：签到禁用，其余关掉；旅行固定整点便于定位。
	s.mutate(func(c *Config) {
		c.CheckinDisabled = true
		c.ActivityDisabled = true
		c.KeepaliveDisabled = true
		c.CatDisabled = true
		c.GrowthDisabled = true
		c.LimitedDisabled = true
		c.RandomDelayMinutes = 0
	})

	if last := s.catchUpMissed(context.Background(), day.Add(8*time.Hour), day.Add(9*time.Hour+40*time.Minute)); last.IsZero() {
		t.Fatal("catchUpMissed 报告没有补跑")
	}
	if n := stub.departCalls.Load(); n != 1 {
		t.Errorf("depart calls=%d want 1（漏跑的旅行槽位应被补跑）", n)
	}
}

// TestCatchUpMissedNoopWhenNothingMissed 没漏跑时不做任何上游调用。
func TestCatchUpMissedNoopWhenNothingMissed(t *testing.T) {
	fastTravel(t)
	stub := &travelStub{buddy: `{"id":1,"name":"喵"}`, state: `{"state":"idle"}`}
	srv := stub.server()
	defer srv.Close()

	s, _ := newTravelScheduler(t, srv, "u1")
	day := time.Date(2026, 10, 27, 0, 0, 0, 0, cstZone)

	if last := s.catchUpMissed(context.Background(), day.Add(9*time.Hour), day.Add(9*time.Hour+3*time.Minute)); !last.IsZero() {
		t.Errorf("没有漏跑槽位却报告补跑了 %s", last)
	}
	if n := stub.infoCalls.Load() + stub.departCalls.Load(); n != 0 {
		t.Errorf("上游调用=%d want 0", n)
	}
}

// TestMissedSlotsCrossMidnight 批次跨过午夜时，前一天深夜的漏跑槽位仍要能扫到
// （扫描窗口从 ranAt 当天零点起算两天，必须覆盖到 now 所在的那一天）。
func TestMissedSlotsCrossMidnight(t *testing.T) {
	s := New(Config{
		CheckinHours:       []int{23},
		TravelHours:        []int{23},
		RandomDelayMinutes: 59,
		RandomDelaySeed:    "cross-midnight",
		ActivityDisabled:   true,
		KeepaliveDisabled:  true,
		CatDisabled:        true,
		GrowthDisabled:     true,
		LimitedDisabled:    true,
	})
	day := time.Date(2026, 10, 27, 0, 0, 0, 0, cstZone)

	ck := s.scheduledSlot(day, 23, taskCheckin)
	tv := s.scheduledSlot(day, 23, taskTravel)
	earlier, later := ck, tv
	laterKind := taskTravel
	if tv.Before(ck) {
		earlier, later = tv, ck
		laterKind = taskCheckin
	}

	// 先跑的那类从 earlier 一直跑到次日 00:30，跨过了 later。
	now := later.Add(30 * time.Minute)
	got := s.missedSlots(earlier, now)
	if len(got) != 1 {
		t.Fatalf("missed=%v want exactly 1 slot", kindsOf(got))
	}
	if got[0].kind != laterKind || !got[0].at.Equal(later) {
		t.Errorf("got %v@%s want %v@%s", got[0].kind, got[0].at, laterKind, later)
	}
}

// TestCatchUpMissedAdvancesToLastSlot 补跑批次自己也可能跨过更后面的槽位：
// 返回值必须是最后补跑的那个槽位，Run 才能拿它当新 ranAt 继续推进。
func TestCatchUpMissedAdvancesToLastSlot(t *testing.T) {
	fastTravel(t)
	stub := &travelStub{buddy: `{"id":1,"name":"喵"}`, state: `{"state":"idle"}`}
	srv := stub.server()
	defer srv.Close()

	s, _ := newTravelScheduler(t, srv, "u1")
	day := time.Date(2026, 10, 27, 0, 0, 0, 0, cstZone)
	s.mutate(func(c *Config) {
		c.CheckinDisabled = true
		c.ActivityDisabled = true
		c.KeepaliveDisabled = true
		c.CatDisabled = true
		c.GrowthDisabled = true
		c.LimitedDisabled = true
		c.RandomDelayMinutes = 0
		c.TravelHours = []int{9, 12}
	})

	// 08:00 唤醒、13:00 才回来：09 与 12 两个旅行槽位都漏了。
	last := s.catchUpMissed(context.Background(), day.Add(8*time.Hour), day.Add(13*time.Hour))
	if want := day.Add(12 * time.Hour); !last.Equal(want) {
		t.Errorf("last=%v want %v（最后补跑的是 12:00 槽位）", last, want)
	}
	if n := stub.departCalls.Load(); n != 2 {
		t.Errorf("depart calls=%d want 2（两个漏跑槽位各跑一趟）", n)
	}
}

// TestCatchUpMissedStopsOnCancel ctx 已取消时不再派发新批次（优雅停机）。
func TestCatchUpMissedStopsOnCancel(t *testing.T) {
	fastTravel(t)
	stub := &travelStub{buddy: `{"id":1,"name":"喵"}`, state: `{"state":"idle"}`}
	srv := stub.server()
	defer srv.Close()

	s, _ := newTravelScheduler(t, srv, "u1")
	s.mutate(func(c *Config) {
		c.CheckinDisabled = true
		c.ActivityDisabled = true
		c.KeepaliveDisabled = true
		c.CatDisabled = true
		c.GrowthDisabled = true
		c.LimitedDisabled = true
	})
	day := time.Date(2026, 10, 27, 0, 0, 0, 0, cstZone)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	s.catchUpMissed(ctx, day.Add(9*time.Hour), day.Add(9*time.Hour+40*time.Minute))
	if n := stub.infoCalls.Load(); n != 0 {
		t.Errorf("info calls=%d want 0（ctx 已取消不该派发）", n)
	}
}
