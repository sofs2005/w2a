// random_schedule.go 定时任务的两类「反模式化」扰动：
//
//  1. 每日时点在**原小时内**随机平移（RandomDelayMinutes）—— 固定整点签到在
//     上游看来是可枚举的机器行为；偏移由稳定哈希得出，重启不重抽、跨部署独立。
//  2. 账号间随机间隔（AccountDelayMin/MaxSeconds）—— 一轮遍历里每两个账号之间
//     额外睡一段，避免 N 个号在几秒内连续打同一接口。
//
// 两条都只在**定时派发**的 ctx 上生效（scheduledAccountKey）：面板/管理接口的
// 手动触发保持零延迟，否则「点一下签到」会莫名其妙卡几分钟。
package scheduler

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math/rand"
	"time"
)

// scheduledAccountKey 定时派发的 ctx 标记。dispatch 注入，accountGap /
// accountScriptArgs 用它区分「定时」与「手动」入口。
type scheduledAccountKey struct{}

// isScheduled 报告 ctx 是否来自定时派发。
func isScheduled(ctx context.Context) bool {
	v, _ := ctx.Value(scheduledAccountKey{}).(bool)
	return v
}

// accountGap 在原有最小间隔 base 上叠加随机秒数；只在定时派发且配置了
// AccountDelayMaxSeconds > 0 时生效。手动调用与 0 区间保持旧节奏。
//
// 配置越界（min<0 / max<min / max>600）时静默回落到 base 而不是 panic：
// config.Normalize 已在启动时挡过，运行期再炸一次只会让调度循环整个停摆。
func (s *Scheduler) accountGap(ctx context.Context, base time.Duration) time.Duration {
	if !isScheduled(ctx) || s.snapshot().AccountDelayMaxSeconds <= 0 {
		return base
	}
	lo, hi := s.snapshot().AccountDelayMinSeconds, s.snapshot().AccountDelayMaxSeconds
	if lo < 0 || hi < lo || hi > 600 {
		return base
	}
	seconds := lo + rand.Intn(hi-lo+1)
	return base + time.Duration(seconds)*time.Second
}

// accountScriptArgs 给 Python 脚本追加账号间隔参数（脚本侧在账号循环里
// AccountSpacing.wait()）。手动入口不追加 —— 保持旧命令行逐字节不变。
func (s *Scheduler) accountScriptArgs(ctx context.Context, args []string) []string {
	if !isScheduled(ctx) || s.snapshot().AccountDelayMaxSeconds <= 0 {
		return args
	}
	return append(args, "--account-delay-min", fmt.Sprint(s.snapshot().AccountDelayMinSeconds),
		"--account-delay-max", fmt.Sprint(s.snapshot().AccountDelayMaxSeconds))
}

// scheduledSlot 返回「day 当天 hour 点这个任务」的实际触发时刻。
//
// 偏移 = sha256(seed | 日期 | 小时 | 任务类型) 前 8 字节 mod (窗口秒数+1)：
//   - 无随机状态可丢，重启后同一天同一槽位仍是同一时刻（否则重启会重抽，
//     「每天 9 点后 N 分钟」变成「每次重启都换一个时间」，反而更可疑）；
//   - 日期/小时/任务类型/部署各进一份哈希 → 日与日之间、任务与任务之间、
//     部署与部署之间的偏移互相独立；
//   - seed 不落日志、不落配置，只有部署方自己知道。
//
// RandomDelayMinutes <= 0 时返回整点（与改造前逐字节一致）。
func (s *Scheduler) scheduledSlot(day time.Time, hour int, kind taskKind) time.Time {
	at := time.Date(day.Year(), day.Month(), day.Day(), hour, 0, 0, 0, day.Location())
	minutes := s.snapshot().RandomDelayMinutes
	if minutes <= 0 {
		return at
	}
	// 偏移必须留在原小时内：60 分钟封顶为 59:59，避免与下一个整点重叠。
	if minutes > 60 {
		minutes = 60
	}
	seconds := minutes * 60
	if seconds >= 3600 {
		seconds = 3599
	}
	identity := fmt.Sprintf("%s\x00%s\x00%d\x00%d", s.snapshot().RandomDelaySeed,
		at.Format("2006-01-02"), hour, kind)
	digest := sha256.Sum256([]byte(identity))
	offset := binary.BigEndian.Uint64(digest[:8]) % uint64(seconds+1)
	return at.Add(time.Duration(offset) * time.Second)
}

// nextScheduledFire 返回 now 之后该任务最近的一次触发时刻（含随机偏移）。
//
// 只扫今天与明天两天：任意 hours 下最近时点必定落在这两天内（与 nextFire
// 的口径一致），多扫无益。未配置随机偏移时直接走 nextFire，保证零回归。
func (s *Scheduler) nextScheduledFire(now time.Time, hours []int, kind taskKind) time.Time {
	if s.snapshot().RandomDelayMinutes <= 0 {
		return nextFire(now, hours)
	}
	var earliest time.Time
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	for i := 0; i < 2; i++ {
		for _, hour := range hours {
			at := s.scheduledSlot(day.AddDate(0, 0, i), hour, kind)
			if at.After(now) && (earliest.IsZero() || at.Before(earliest)) {
				earliest = at
			}
		}
	}
	return earliest
}
