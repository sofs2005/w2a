// travel.go 猫猫旅行巡检状态机：随旅行时点（travel_hours，默认 09 点）对池内每个可用账号单趟推进一次。
// 无猫 → 同意协议 + 领养；有猫 → 按 travel/status 分派 派出 / 领奖 / 跳过。
package scheduler

import (
	"context"
	"log"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/logfmt"
	"workbuddy2api/internal/upstream"
)

const (
	// travelLocationID 派出地点固定 4（古镇客栈）：4 个地点收益/时长区间完全相同，无最优解。
	travelLocationID = 4

	// travelStateIdle 空闲可派出；travelStateTraveling 在途；travelStateArrived 到站可领奖。
	travelStateIdle      = "idle"
	travelStateTraveling = "traveling"
	travelStateArrived   = "arrived"
)

// travelAccountDelay 账号间限速：全量账号约 40s，避免上游风控。测试可置 0。
var travelAccountDelay = 800 * time.Millisecond

// activityAccountDelay 活跃上报账号间限速：与旅行同口径，避免上游风控。测试可置 0。
var activityAccountDelay = 800 * time.Millisecond

// activityReportGap 同一账号内连续上报之间的间隔：5 连发模拟同一会话多轮对话，
// 秒发易触发风控，故 1.5s 一条。测试可置 0。
var activityReportGap = 1500 * time.Millisecond

// sleepCtx 可取消的等待：ctx 取消立即返回 false（优雅停机不必等 sleep 醒来），
// 等满返回 true。d<=0 立即放行（测试把延迟置 0 时不白等）。
// 替换 time.Sleep：账号间/账号内限速值不变，只换等待方式。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// cstZone 上游每日重置按自然日 00:00 CST（Asia/Shanghai）。中国无夏令时，固定 +8 即可，
// 不依赖容器 tzdata。
var cstZone = time.FixedZone("CST", 8*60*60)

// travelDay 返回 t 所属的上游自然日（CST），格式 2006-01-02。
func travelDay(t time.Time) string {
	return t.In(cstZone).Format("2006-01-02")
}

// RunTravelNow 立即对池内所有可用账号执行一趟旅行巡检（无 ctx 的外部入口：
// 测试与潜在的手动触发——cmd 侧从未接线，不存在 cmd/travel 入口，部署验证
// 场景由 RunActivityNow 覆盖）。内部走 runTravel，取背景 ctx（不可取消，
// 语义与引入前 time.Sleep 版一致）。
func (s *Scheduler) RunTravelNow() {
	s.runTravel(context.Background())
}

// runTravel 旅行巡检遍历，随 ctx 取消立即退出。
// 禁用账号跳过；401/查询失败只跳过该账号本轮（不强刷 token，交 22:00 keepalive）；
// 账号间限速 travelAccountDelay（sleepCtx：取消时立即放弃后续账号）。
//
// 收尾打一行汇总：本方法此前**只有逐账号日志、没有整体收尾**，于是
// 「一趟跑完但每个账号都 skip」与「这趟根本没跑」在日志里同样表现为
// 「没有 depart ok」——排查时无法区分（正是 issue 里「旅行好像没在运行」
// 的观测盲区）。汇总行给出参与账号数与各动作计数，让每趟都有明确证据。
func (s *Scheduler) runTravel(ctx context.Context) {
	first := true
	var seen, departN, claimN, adoptN, skipN int
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshTokenValue() == "" {
			log.Printf("travel %s: skip (no credentials)", logfmt.Label(st.UID, st.Nickname))
			continue
		}
		if a.IsGlobal() {
			// D4 门控：global 无猫猫旅行体系，不发起任何上游调用。
			// 逐账号打点：此跳过原先完全静默，排查「旅行是不是没跑」时日志里
			// 连一行都没有，与「跑了但全是 skip」无法区分（与签到 skip 同口径）。
			log.Printf("travel %s: skip (global realm)", logfmt.Label(st.UID, st.Nickname))
			continue
		}
		if !first {
			if !sleepCtx(ctx, travelAccountDelay) {
				return // 优雅停机：不等限速睡满，剩余账号下轮再巡
			}
		}
		first = false
		seen++
		switch s.travelOne(a) {
		case travelActDepart:
			departN++
		case travelActClaim:
			claimN++
		case travelActAdopt:
			adoptN++
		default:
			skipN++
		}
	}
	if seen > 0 {
		log.Printf("travel done: accounts=%d depart=%d claim=%d adopt=%d skip=%d",
			seen, departN, claimN, adoptN, skipN)
	}
}

// travelAct 单账号单趟的动作结果（供 runTravel 汇总计数，不影响日志）。
type travelAct int

const (
	travelActSkip   travelAct = iota // 跳过（在途 / 当日已达上限 / 查询失败）
	travelActDepart                  // 派出成功
	travelActClaim                   // 领奖成功
	travelActAdopt                   // 领养成功
)

// travelOne 单账号单趟状态机：查有无猫 + 查状态 + 最多一个动作，不轮询不等待。
// 返回本趟实际动作，供 runTravel 汇总。
func (s *Scheduler) travelOne(a *auth.Auth) travelAct {
	buddy, err := s.cfg.Upstream.BuddyInfo(a)
	if err != nil {
		log.Printf("travel %s: buddy-info: %v", logfmt.Label(a.UID, a.Nickname), err)
		return travelActSkip
	}
	if buddy == nil {
		return s.travelAdopt(a)
	}
	ts, err := s.cfg.Upstream.TravelStatus(a)
	if err != nil {
		log.Printf("travel %s: status: %v", logfmt.Label(a.UID, a.Nickname), err)
		return travelActSkip
	}
	switch ts.State {
	case travelStateArrived:
		return s.travelClaim(a, ts)
	case travelStateIdle:
		return s.travelDepart(a, ts)
	case travelStateTraveling:
		log.Printf("travel %s: skip (traveling record=%d)", logfmt.Label(a.UID, a.Nickname), ts.RecordID)
		return travelActSkip
	default:
		log.Printf("travel %s: skip (unknown state %q)", logfmt.Label(a.UID, a.Nickname), ts.State)
		return travelActSkip
	}
}

// travelDepart 空闲且未达当日上限时派出（每日 1 次，自然日 00:00 CST 重置）。
func (s *Scheduler) travelDepart(a *auth.Auth, ts *upstream.TravelState) travelAct {
	if ts.DailyLimitReached {
		log.Printf("travel %s: skip (daily limit reached)", logfmt.Label(a.UID, a.Nickname))
		return travelActSkip
	}
	if err := s.cfg.Upstream.TravelDepart(a, travelLocationID); err != nil {
		log.Printf("travel %s: depart: %v", logfmt.Label(a.UID, a.Nickname), err)
		return travelActSkip
	}
	log.Printf("travel %s: depart ok location=%d", logfmt.Label(a.UID, a.Nickname), travelLocationID)
	return travelActDepart
}

// travelClaim 到站领奖（必须带 record_id）。
func (s *Scheduler) travelClaim(a *auth.Auth, ts *upstream.TravelState) travelAct {
	if ts.RecordID == 0 {
		log.Printf("travel %s: claim skipped (arrived but no record_id)", logfmt.Label(a.UID, a.Nickname))
		return travelActSkip
	}
	reward, err := s.cfg.Upstream.TravelClaim(a, ts.RecordID)
	if err != nil {
		log.Printf("travel %s: claim record=%d: %v", logfmt.Label(a.UID, a.Nickname), ts.RecordID, err)
		return travelActSkip
	}
	log.Printf("travel %s: claim ok record=%d reward=%d", logfmt.Label(a.UID, a.Nickname), ts.RecordID, reward)
	return travelActClaim
}

// travelAdopt 旅行巡检时领养：受 adoptTriedToday 当日防抖约束。
func (s *Scheduler) travelAdopt(a *auth.Auth) travelAct {
	return s.adoptBuddy(a, false)
}

// travelAdoptForce 活跃上报补满对话量后领养：豁免 adoptTriedToday 当日防抖。
// 背景：旅行排程 09 点已领养且因对话量未达 skip，10 点活跃上报 5 连发把
// 对话量补满——此时是「门槛刚达成」的新状态，不算对上游重试轰炸，放行重试。
// 有猫账号 BuddyInfo 非空时直接跳过（不重复领养）。
func (s *Scheduler) travelAdoptForce(a *auth.Auth) {
	buddy, err := s.cfg.Upstream.BuddyInfo(a)
	if err != nil {
		log.Printf("activity %s: buddy-info: %v", logfmt.Label(a.UID, a.Nickname), err)
		return
	}
	if buddy != nil {
		return // 已有猫，无需领养
	}
	s.adoptBuddy(a, true) // force=true 豁免当日防抖（返回值仅 runTravel 汇总用，此处忽略）
}

// adoptBuddy 无猫时领养：先同意协议（幂等）再 buddy/first。
// conversation 门槛未达标（HTTP 400 first_buddy task not completed yet）属预期行为，
// 记一次当日已试后静默跳过，不再重试。force=true 时豁免当日防抖（活跃上报补满对话量后重试）。
// 返回本趟动作（领养成功 / 跳过），供 runTravel 汇总——travelAdoptForce 忽略返回值。
func (s *Scheduler) adoptBuddy(a *auth.Auth, force bool) travelAct {
	if !force && s.adoptTriedToday(a.UID) {
		return travelActSkip
	}
	if err := s.cfg.Upstream.BuddyAgreement(a); err != nil {
		log.Printf("travel %s: agreement: %v", logfmt.Label(a.UID, a.Nickname), err)
		return travelActSkip
	}
	err := s.cfg.Upstream.BuddyFirst(a)
	switch {
	case err == nil:
		log.Printf("travel %s: adopt ok (+300 credits)", logfmt.Label(a.UID, a.Nickname))
		return travelActAdopt
	case upstream.IsBuddyTaskIncomplete(err):
		s.markAdoptTried(a.UID)
		log.Printf("travel %s: adopt skipped (conversation threshold not reached, retry tomorrow)", logfmt.Label(a.UID, a.Nickname))
		return travelActSkip
	default:
		log.Printf("travel %s: adopt: %v", logfmt.Label(a.UID, a.Nickname), err)
		return travelActSkip
	}
}

// adoptTriedToday 该账号当日是否已判定领养门槛未达。
func (s *Scheduler) adoptTriedToday(uid string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.adoptTried[uid] == travelDay(time.Now())
}

// markAdoptTried 记录该账号当日已尝试领养且未过门槛。
func (s *Scheduler) markAdoptTried(uid string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.adoptTried[uid] = travelDay(time.Now())
}
