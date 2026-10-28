// persist.go 跨重启的调度状态：只记一件事——「已处理到的最后一个槽位时刻」，
// 以及启动时据此补跑停机期间被跨过的槽位。
//
// 为什么需要落盘：Run 的启动流程是从"现在"往后排（nextWake(time.Now())），而
// nextScheduledFire 只挑 at.After(now) 的时点——停机期间过去的槽位一律不补。
// 线上实案：容器 2026-10-27 12:49:38 创建，而 activity_hours=[10]（随机偏移 26
// 分钟，实际槽位落在 10:00–10:26）→ 槽位时刻早于进程启动时刻，当天永久丢失，
// docker logs 里 activity 相关行一条都没有。进程内补跑（missedSlots/catchUpMissed）
// 修的是"批次跑过头"，跨重启的这一半只能靠落盘才知道上次干到哪儿。
//
// 为什么只落一个时间戳、而不是"每类任务上次跑的时刻"：槽位时刻是 hours + 随机偏移
// 算出来的纯函数（scheduledSlot），知道上次处理到哪儿，重算一遍就能列出中间漏了
// 哪些（见 startupCatchUp）。多存一份每类状态只会多出一份可能与代码对不上的真相。
//
// 失败语义（读坏/写不进去都不能让调度停摆——停摆比漏跑一次严重得多）：
//   - 文件不存在：首次部署的正常态，静默按"没有历史"处理，不打 WARN；
//   - 内容损坏/字段非法：一行 WARN 后按"没有历史"处理（不补跑，宁缺勿错）；
//   - 写失败：一行 WARN（sync.Once 只报一次，避免每轮批次刷屏），继续调度。
package scheduler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// schedulerStateVersion 状态文件 schema 版本。读到别的版本按"损坏"处理：
// 未来版本可能改了字段含义，用旧代码猜着读比不读更危险（会算出一个错的补跑区间）。
const schedulerStateVersion = 1

// StateFileName 状态文件名。装配层（cmd/server 的 statePathFor）用它拼出完整路径，
// 放在 state_file 的同目录：与 pool 的 state.json、模型目录等运维文件同处一地，
// 备份/迁移一个 data 目录即可带走全部进程状态。
//
// 导出而非各自写字面量：两处各写一遍"文件名"，改一处漏一处就会静默读到空文件
// （表现为"每次启动都当成首次部署"，不补跑也不报错——最难查的那类故障）。
const StateFileName = "scheduler_state.json"

// schedulerState 落盘结构。last_wake 用字符串而非 time.Time：time.Time 的 JSON
// 编码是 RFC3339Nano 但时区行为随版本/来源浮动（手工编辑过就成"0001-01-01T00:00:00Z"），
// 显式字符串 + 自己 Parse 能给出可读的 WARN（"last_wake 非 RFC3339"）。
type schedulerState struct {
	Version  int    `json:"version"`
	LastWake string `json:"last_wake"` // RFC3339，带时区，如 2026-10-27T22:14:40+08:00
}

// loadLastWake 读取"已完成处理的最后一个槽位时刻"。
//
// ok=false 覆盖三种情况（首次部署 / 内容损坏 / 版本不支持），调用方一律按
// "没有历史"处理——即不补跑。为什么不猜一个保守值（比如 now-1h）继续补：猜错
// 的后果是把昨天跑过的任务再打一遍上游，而"不补"最坏只是漏一次、明天照常。
func loadLastWake(path string) (time.Time, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// 首次部署（或运维清了 data 目录）：静默，不打 WARN——新装用户看到
			// "状态文件读不到"只会以为装坏了。
			return time.Time{}, false
		}
		log.Printf("WARN: [sched] 调度状态读取失败（%s）：%v —— 按首次部署处理，本次启动不补跑", path, err)
		return time.Time{}, false
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		// 空文件：多为"写到一半被 kill"（tmp+rename 之前的旧实现）或运维误 touch。
		log.Printf("WARN: [sched] 调度状态文件为空（%s）—— 按首次部署处理，本次启动不补跑", path)
		return time.Time{}, false
	}
	var st schedulerState
	if err := json.Unmarshal(raw, &st); err != nil {
		log.Printf("WARN: [sched] 调度状态解析失败（%s）：%v —— 按首次部署处理，本次启动不补跑", path, err)
		return time.Time{}, false
	}
	if st.Version != schedulerStateVersion {
		log.Printf("WARN: [sched] 调度状态版本不支持（%s: version=%d want %d）—— 按首次部署处理，本次启动不补跑",
			path, st.Version, schedulerStateVersion)
		return time.Time{}, false
	}
	at, err := time.Parse(time.RFC3339, strings.TrimSpace(st.LastWake))
	if err != nil {
		log.Printf("WARN: [sched] 调度状态 last_wake 非 RFC3339（%s: %q）：%v —— 按首次部署处理，本次启动不补跑",
			path, st.LastWake, err)
		return time.Time{}, false
	}
	// 零值/异常年份按"损坏"处理：time.Time 的零值编码成 "0001-01-01T00:00:00Z"，
	// 它是**合法 RFC3339**，光靠 Parse 判不出来（见上面 schedulerState 的注释：
	// 手工编辑过就会写成这个样子）。若采信它，它会被当成"很久以前处理过"→ 补跑
	// 窗口被 lookback 夹到最近 12 小时 → 每次启动都把窗口内的槽位补一遍，与本文件
	// 开头声明的"读坏就按没有历史处理、不补跑"正好相反。
	//
	// Year() < 2000 的理由：这个项目 2026 年才上线，任何真实部署的槽位时刻都不可能
	// 早于 2000 年；出现这种年份只可能是"手改过/写坏了"（或时钟被拨到了石器时代）。
	// 此时宁可不补——最坏漏一次、明天照常；而按假历史补跑的代价是把当天的任务提前
	// 打一遍上游，且会被上游当成异常行为。
	if at.IsZero() || at.Year() < 2000 {
		log.Printf("WARN: [sched] 调度状态 last_wake 为零值/异常年份（%s: %q）—— 按首次部署处理，本次启动不补跑", path, st.LastWake)
		return time.Time{}, false
	}
	return at, true
}

// saveLastWake 原子落盘"已处理到的槽位时刻"。
//
// tmp + rename（与 auth.SaveAtomic 的 refresh 落盘同一做法）：直接覆盖写的话，
// 进程在写一半时被 kill 会留下半截 JSON，下次启动就退化成"没有历史"（永久丢失
// 一次补跑机会）。rename 在同一文件系统上是原子的，读到的永远是完整的一版。
// 0600：槽位时刻能反推出部署的随机偏移规律（seed 不落盘，但时刻本身也是信息）。
func saveLastWake(path string, at time.Time) error {
	doc := schedulerState{
		Version:  schedulerStateVersion,
		LastWake: at.Format(time.RFC3339),
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	// 目录不存在就建：StatePath 由装配层从 state_file 派生，正常情况下 data/ 已存在；
	// 但 pool 用空 state_file 启动时（测试/无状态模式）目录可能确实没有，此时补建
	// 比让状态永远写不进去好。已存在时 MkdirAll 是零成本的一次 stat。
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	// tmp 名带 pid：单写者成立时与固定名没有实际差别，但两个进程共享同一 data 目录
	// （误开了第二个实例、备份/迁移脚本顺手写）时，固定 tmp 名会让两边的 O_TRUNC +
	// 分次 write 交织成"混合内容"——rename 之后落地的那份既不是 A 写的也不是 B 写的。
	// 带 pid 后各写各的 tmp，rename 仍是原子的，最终落地的一定是某个进程的完整版本。
	tmp := fmt.Sprintf("%s.tmp.%d", path, os.Getpid())
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// persistLastWake 把"已处理到的槽位时刻"落盘。
//
// StatePath 为空 = 本特性关闭（零值 Config / 测试 / 降级），直接返回：调用方
// （Run / startupCatchUp）因此不必到处判空，行为与引入持久化之前逐字节一致。
//
// 写失败只报一次：调度器每次批次完成都会调本方法，磁盘满/只读挂载时若每次都
// 打 WARN，日志会被这条淹没，真正的任务日志反而看不见。一次提示足够让运维去
// 查挂载；后果（重启后重跑一次已跑过的槽位）写进 WARN 正文。
//
// 单写者：只在 Run 的 goroutine（含它调用的 startupCatchUp）里调用，不加锁；
// sync.Once 本身并发安全，重复调用只是重复写同一个值。
func (s *Scheduler) persistLastWake(at time.Time) {
	path := s.snapshot().StatePath
	if path == "" || at.IsZero() {
		return
	}
	if err := saveLastWake(path, at); err != nil {
		s.stateWarnOnce.Do(func() {
			log.Printf("WARN: [sched] 调度状态落盘失败（%s）：%v —— 本次运行不再提示；"+
				"后果是重启后可能把已跑过的槽位再跑一遍", path, err)
		})
	}
}

// persistLastWakeIfRunning 批次跑完后的落盘，仅在 ctx 未取消时执行。
//
// 为什么要这一层判断：runBatch 在 ctx 取消时会提前返回（串行派发时逐类检查
// ctx.Err()，并行派发时各任务内部的 sleepCtx 快速收尾），批次只跑了一部分。
// 此时若照常落盘，这个槽位就被标记成"已完成"，剩下没跑的账号要等下一个自然日
// 才会再被处理——方向与"宁可重复一次，不可漏掉一天"正好相反（该重复的没重复）。
// 所以 ctx 已取消时**不落盘**：状态停在上一个真正跑完的槽位，留给下次启动补跑。
//
// 抽成独立方法而不是在 Run 里内联 if：Run 的落盘点在 waitSlot 之后，测试不等待
// 真实槽位就走不到那里；有了这个方法，测试可以用"已取消的 ctx + 预置状态文件"
// 直接验证"不落盘"，不必给 Run 注入时钟或信号通道（那会改动 Run 的签名）。
func (s *Scheduler) persistLastWakeIfRunning(ctx context.Context, at time.Time) {
	if ctx.Err() != nil {
		// 优雅停机时不把半截批次标记为完成：runBatch 在 ctx 取消时提前返回，
		// 剩下没跑的账号必须留给下次启动补跑（宁可重复一次，不可漏掉一天）。
		return
	}
	s.persistLastWake(at)
}

// startupCatchUpLookback 启动补跑的回溯窗口上限（12 小时）。
//
// 取值理由：覆盖两种最常见的停机形态——"停机一夜后早上起来"（昨天 22 点停机、
// 今早 8 点起来，跨过 01:00 夜猫与 08:00 成长）与"下午重启错过上午槽位"
// （10:00 活跃、12:00 限时）。同时天然挡掉"停机三天后补一整天历史任务"：
// last_wake 再老也只回看到 12 小时前，最多补到昨夜今晨的槽位，而不是把三天的
// 签到/旅行/上报全部重放一遍（那会瞬间打满上游，且昨天的任务今天补也没有意义）。
const startupCatchUpLookback = 12 * time.Hour

// startupCatchUp 启动补跑入口：用真实当前时刻。
//
// 返回值是**最后一个被补跑的槽位时刻**（零值 = 本次没补任何东西）。它不是日志装饰：
// 启动补跑自己派发的批次同样可能跑很久（生产 account_delay_min/max=220/600s × 7 个
// 账号 → 一轮签到 22~60 分钟），足以跨过更后面的槽位（09:00 的签到补跑跨过 10:00 的
// 活跃上报是常态，不是边角）。把"补到哪儿了"交回 Run，才能接进同一个 catch-up 循环
// 把被跨过的槽位也在**本次启动**补掉，而不是等下一次重启——后者在生产上等于丢一天。
func (s *Scheduler) startupCatchUp(ctx context.Context) time.Time {
	return s.startupCatchUpAt(ctx, time.Now())
}

// startupCatchUpAt 补跑 [max(lastWake, now-lookback), now] 内被跨过的槽位。
//
// 与进程内补跑（catchUpMissed）的分工：那个修的是"同一进程里批次跑过头"，这个
// 修的是"槽位落在进程根本没活着的时段"。两者判据同源（missedSlots），区别只在
// 下界从"上次唤醒时刻"换成"落盘的 last_wake"。
//
// 折叠：按任务类型只保留最新的一个槽位（latestPerKind）。昨天 21 点的签到在今天
// 8 点启动时补跑是纯浪费——签到按自然日幂等，补昨天的槽位实际执行的就是今天的
// 签到；同类更早的槽位已被更近的那个完全取代。折叠后成本上界 = 七类各最多一轮，
// 停机三天与停机一夜的补跑代价相同（都受 lookback 约束）。
//
// now 由参数传入而非内部取 time.Now()：测试要固定"容器 12:49 启动、槽位 10:00"
// 这种时刻关系，注入参数比换全局时钟干净。
//
// 返回值 = 最后一个被补跑的槽位时刻（零值 = 一个都没补）。ctx 提前取消时返回**已完成
// 的最后一个**（而非零值）：前面的槽位确实跑完并落了盘，Run 拿它当 ranAt 继续查漏是
// 正确的；返回零值反而会让调用方以为"什么都没发生"而丢掉这段进度。
//
// 注意最后一个可能**没有落盘**：批次执行中途 ctx 被取消时，该槽位是"半截批次"，
// persistLastWakeIfRunning 刻意不推进它（否则剩下没跑的账号要等下一个自然日）。
// 于是返回值最多比落盘的 last_wake 多一个槽位。Run 不受影响——ctx 已取消时它的
// catch-up 循环守卫（ctx.Err() == nil）不会进；真正会用到这个返回值差异的是**下一次
// 启动**，而那次读的是落盘的 last_wake，所以该槽位会被重新补跑一次（宁可重复一次，
// 不可漏掉一天）。
func (s *Scheduler) startupCatchUpAt(ctx context.Context, now time.Time) time.Time {
	cfg := s.snapshot()
	if cfg.StatePath == "" {
		// 未接线（零值 Config / 显式降级）：不读不写不补，行为与引入前完全一致。
		return time.Time{}
	}
	lastWake, ok := loadLastWake(cfg.StatePath)
	if !ok {
		// 首次部署 / 状态损坏：没有可信的历史，一律不补（见 loadLastWake 注释）。
		return time.Time{}
	}
	// 状态文件带时区（可能是上次运行在别的 TZ 下写的）：槽位是按**进程本地时区**
	// 定义的（scheduledSlot 用 day.Location()），折算到本地再算，否则跨时区迁移
	// 会让"21 点"错位成另一个时刻。
	lastWake = lastWake.In(now.Location())

	from := lastWake
	if floor := now.Add(-startupCatchUpLookback); floor.After(from) {
		from = floor
	}
	if !from.Before(now) {
		// last_wake 在未来：时钟回拨、时区变更、或运维手改了状态文件。补跑区间为空
		// 甚至倒挂，算出来的槽位不可解释——直接放弃本次补跑，等主循环正常排程。
		log.Printf("WARN: [sched] 调度状态 last_wake=%s 不早于当前时刻 %s，跳过启动补跑（时钟回拨或时区变更？）",
			lastWake.Format(time.RFC3339), now.Format(time.RFC3339))
		return time.Time{}
	}

	missed := latestPerKind(s.missedSlots(from, now))
	if len(missed) == 0 {
		// 只在确实读到历史时打这行：首次部署静默，否则每个新装用户都会看到一条
		// "nothing missed"（正确但无信息量）。
		log.Printf("[sched] startup catch-up: nothing missed since %s", from.Format("2006-01-02 15:04:05"))
		return time.Time{}
	}
	log.Printf("[sched] startup catch-up: from=%s slots=%v",
		from.Format("2006-01-02 15:04:05"), slotLabels(missed))

	var last time.Time
	for i := 0; i < len(missed); {
		at := missed[i].at
		var kinds []taskKind
		// 同一时刻的多类任务合并成一批：与 nextWake / catchUpMissed 的口径一致
		// （同槽多类本就该一起跑，拆成两批只是白等两轮账号间隔）。
		for ; i < len(missed) && missed[i].at.Equal(at); i++ {
			kinds = append(kinds, missed[i].kind)
		}
		if ctx.Err() != nil {
			// 优雅停机：剩余补跑放弃。走到这里说明上一批是**跑完且已落盘**的：
			// 批次中途被取消时 persistLastWakeIfRunning 不推进 last_wake，下面那句
			// ctx 检查会直接返回，压根回不到循环顶部。所以"已跑完的批次已经落过盘、
			// 不会重复"成立，而"被取消的那一批没落盘、下次启动会补"同样成立。
			return last
		}
		log.Printf("[sched] startup catch-up: dispatch slot %s kinds=%v",
			at.Format("2006-01-02 15:04:05"), kinds)
		s.runBatch(ctx, kinds)
		last = at
		// 每批跑完立即推进 last_wake：下一批（或下一次重启）从这里继续。语义刻意是
		// "批次跑完才推进"——批次中途被 kill 时该槽位下次启动会重跑一次，代价见
		// persistLastWake 与 Run 的注释（七类任务都按天幂等，宁可重复不可漏掉一天）。
		//
		// 但"跑完"必须是真跑完：runBatch 在 ctx 取消时提前返回（批次只跑了一部分），
		// 此时落盘等于把半截批次标记成已完成，剩下没跑的账号要等下一个自然日才被
		// 处理。所以走 persistLastWakeIfRunning，ctx 已取消就不推进，该槽位留给下次
		// 启动补跑（见本方法末尾对返回值的说明）。
		s.persistLastWakeIfRunning(ctx, at)
		if ctx.Err() != nil {
			return last
		}
	}
	return last
}

// latestPerKind 按任务类型折叠补跑候选：每类只留最新的一个槽位，返回结果仍按
// 槽位时刻升序（跨类顺序保持"先发生的先跑"，与 missedSlots 的口径一致）。
//
// 同一时刻的多个类各自保留（它们的 at 相同，仍会被合并成一批）。同刻时按
// taskKind 声明序排序，让日志与测试断言稳定（map 遍历顺序是随机的）。
func latestPerKind(missed []missedSlot) []missedSlot {
	latest := make(map[taskKind]missedSlot, len(missed))
	for _, m := range missed {
		if prev, ok := latest[m.kind]; !ok || m.at.After(prev.at) {
			latest[m.kind] = m
		}
	}
	out := make([]missedSlot, 0, len(latest))
	for _, m := range latest {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].at.Equal(out[j].at) {
			return out[i].kind < out[j].kind
		}
		return out[i].at.Before(out[j].at)
	})
	return out
}

// slotLabels 把槽位列表渲染成 `[activity@2026-10-27 10:00:00 ...]` 供日志阅读。
//
// 不直接 %v 打 []missedSlot：那会输出带单调时钟与 Location 指针的完整结构体，
// 一条日志几十个字段，排障时反而看不出"哪类任务、什么时候"。
func slotLabels(missed []missedSlot) []string {
	out := make([]string, 0, len(missed))
	for _, m := range missed {
		out = append(out, fmt.Sprintf("%v@%s", m.kind, m.at.Format("2006-01-02 15:04:05")))
	}
	return out
}
