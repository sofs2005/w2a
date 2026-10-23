// school.go 夜猫子任务、成长任务补跑与限时活动任务的脚本类排程：从系统 crontab 迁入 Go scheduler。
//
// 背景：cat（01:00 夜猫窗口）原由系统 crontab 调 scripts/school_open_day_cron.sh
// 执行——依赖外部系统 cron、容器重建可能丢失、不在 config 里配置。迁入后成为独立
// 任务，时点由 schedule.cat_hours 配置。
//
// 开学季排程（原第五类 school）已被限时活动排程取代：开学季本身就是一次性限时活动，
// 活动结束后（in_period=false）不该继续占一个固定排程位。school_open_day_2026.py
// 与 school_open_day_cron.sh 保留为手动入口，脚本自身在活动下线后全量跳过。
//
// growth（08:00）与 limited（12:00）都是 task_runner.py 的入口，见 RunGrowthTasksNow /
// RunLimitedTasksNow。
package scheduler

import (
	"context"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// repoRoot 定位仓库根（容器内 /app、宿主 /root/w2a）。
// 策略：从当前工作目录逐级向上找 scripts/school_open_day_2026.py，
// 找不到回落 os.Getwd()（此时 Run 会因脚本缺失打 WARN，不 panic）。
// 注意：Go scheduler 在 cmd/server 内以工作目录启动（容器 WORKDIR /app），
// 若进程以别的工作目录拉起（如 systemd/裸 binary），上溯穷尽后仍以
// os.Getwd() 兜底，把缺失暴露成 WARN 而非静默。
func repoRoot() string {
	start, err := os.Getwd()
	if err != nil {
		return "."
	}
	dir := start
	for {
		if _, err := os.Stat(filepath.Join(dir, "scripts", "school_open_day_2026.py")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return start
		}
		dir = parent
	}
}

// scriptRunner 脚本子进程的最小执行面：可被测试替换，避免测试真正拉起 python3。
type scriptRunner interface {
	SetDir(string)
	Run() error
}

// contextSetter 可选能力：支持 ctx 取消的实现（scriptCmd 实现；测试 fake 不必）。
// 不并进 scriptRunner 主接口，是为了让既有 fake 无需改动。
type contextSetter interface {
	SetContext(context.Context)
}

// scriptCmd exec.Cmd 适配器：把 exec.Cmd 的 Dir 字段包装成 SetDir 方法，
// 满足 scriptRunner 接口（exec.Cmd 本身只有字段没有方法）。
type scriptCmd struct{ cmd *exec.Cmd }

func (c *scriptCmd) SetDir(dir string) { c.cmd.Dir = dir }

// SetContext 用同一个可执行文件与参数重建为 CommandContext：进程在 ctx 取消时
// 被杀掉，避免停机时残留 python 子进程继续打上游。
func (c *scriptCmd) SetContext(ctx context.Context) {
	dir := c.cmd.Dir
	c.cmd = exec.CommandContext(ctx, c.cmd.Path, c.cmd.Args[1:]...)
	c.cmd.Dir = dir // 保留已设置的工作目录（调用顺序 SetDir→SetContext 也成立）
}

func (c *scriptCmd) Run() error { return c.cmd.Run() }

// newScriptCmd 构建脚本子进程。包级变量便于测试注入 fake（installFakeExec 覆盖）。
// 工作目录由调用方 SetDir 显式设置仓库根。
var newScriptCmd = func(program string, args ...string) scriptRunner {
	return &scriptCmd{cmd: exec.Command(program, args...)}
}

// pythonCmd 返回执行 scripts/*.py 的解释器名。
//
// 默认 "python3"，与容器/Linux 现状完全一致，行为零变更；WB2A_PYTHON
// 显式指定时优先，供解释器不叫 python3 的环境使用（命名对齐仓库 Go 侧
// WB2A_* env 约定，如 WB2A_AUTH_DIR / WB2A_LISTEN）。
//
// 需要该开关的原因：Windows 官方安装器只提供 python.exe，且 PATH 上常存在
// Microsoft Store 的 python3.exe App Execution Alias 存根——exec.Command 能找到
// 它却无法真正执行，脚本类任务统一报 `exit status 9009`。
// 设 WB2A_PYTHON=python 即可绕过。
func pythonCmd() string {
	if v := strings.TrimSpace(os.Getenv("WB2A_PYTHON")); v != "" {
		return v
	}
	return "python3"
}

// runScript 依次执行若干脚本命令：任一命令失败只记一行 WARN，不向上抛、
// 不影响调度主循环继续跑下一个时点。单命令失败不中断后续命令。
func runScript(name, root string, commands [][]string) {
	runScriptContext(context.Background(), name, root, commands)
}

// runScriptContext 同 runScript，但把 ctx 传给子进程：ctx 取消即杀子进程，
// 且取消后不再启动后续命令（优雅停机不留下半截脚本）。
func runScriptContext(ctx context.Context, name, root string, commands [][]string) {
	for _, cmdArgs := range commands {
		if ctx.Err() != nil {
			return
		}
		c := newScriptCmd(cmdArgs[0], cmdArgs[1:]...)
		if setter, ok := c.(contextSetter); ok {
			setter.SetContext(ctx)
		}
		c.SetDir(root)
		if err := c.Run(); err != nil {
			log.Printf("WARN: %s (%s): %v", name, cmdArgs[1], err)
			continue
		}
		log.Printf("%s: ok (%s)", name, cmdArgs[1])
	}
}

// RunCatNow 立即执行夜猫子任务：task_runner.py ALL --yes --only black_cat。
// black_cat 时段敏感：夜猫窗口 23:00–08:00 CST 内最多补 1 次（task_runner 内部
// 判定，非窗口期打印 skip 正常退出）。失败只记 WARN。
func (s *Scheduler) RunCatNow() { s.runCatNow(context.Background()) }

func (s *Scheduler) runCatNow(ctx context.Context) {
	taskRunnerMu.Lock()
	defer taskRunnerMu.Unlock()
	root := repoRoot()
	runScriptContext(ctx, "cat", root, [][]string{
		s.accountScriptArgs(ctx, []string{pythonCmd(), "scripts/task_runner.py", "ALL", "--yes", "--only", "black_cat"}),
	})
}

// taskRunnerMu 串行化 task_runner.py 的两个排程入口（夜猫子 / 成长任务补跑）。
//
// 为什么需要：两者是同一个脚本、同一批 auth 文件。默认时点（cat 01:00 / growth 08:00）
// 不会撞，但用户可以配到同一小时——那时刻 runBatch 会把两者丢进并行的 goroutine，
// 两个 python 进程同时读写同一份 auth 目录与任务状态，结果不确定。
// 锁只覆盖 task_runner.py 自身；school 脚本不共享它要的包级状态，不进这把锁。
var taskRunnerMu sync.Mutex

// RunGrowthTasksNow 立即执行成长任务补跑：task_runner.py ALL --yes。
//
// 与 RunCatNow 的分工：cat 只管时段敏感的 black_cat（--only 限定），本入口跑全量
// 23 项映射任务——其中 16 项是**一次性**成长任务（first_buddy / create_canvas /
// chat_5 / expert_5 / Buddy_App …），账号跑过一次就永久达标，此前没有任何排程挂
// 这个脚本，新号入库后奖励一直躺在"可点亮"状态无人领。脚本自身幂等（已 claimed /
// 已达标的账号逐项 skip），每日扫一遍即天然覆盖新账号，无需维护"已跑过"状态。
//
// 三项由脚本内部规则自行处置、排程侧不必分支：
//   - black_cat：非夜猫窗口（23-08 CST）打印 skip pending 正常退出；即便用户把
//     growth_hours 配进窗口，补跑自身的 cap 限制（单次最多补 1 次）与 cat 排程的
//     已达标即跳过共同兜底，重叠无非幂等写。
//   - Expert_Philanthropy：MAPPING 标注 unforgeable（真实捐款），skip 并注明原因。
//   - global realm 账号：脚本 auth_is_global 门控跳过，不发起任何 CN 任务中心请求。
//
// 失败只记 WARN（runScript 口径），不影响同槽其余任务族。
func (s *Scheduler) RunGrowthTasksNow() { s.runGrowthTasksNow(context.Background()) }

func (s *Scheduler) runGrowthTasksNow(ctx context.Context) {
	taskRunnerMu.Lock()
	defer taskRunnerMu.Unlock()
	root := repoRoot()
	runScriptContext(ctx, "growth", root, [][]string{
		s.accountScriptArgs(ctx, []string{pythonCmd(), "scripts/task_runner.py", "ALL", "--yes"}),
	})
}

// RunLimitedTasksNow 立即执行限时活动任务：task_runner.py ALL --yes --limited。
//
// 与 growth 的分工：growth 跑长期的一次性成长任务（16 项），本入口只跑 MAPPING
// 里带 valid_end 的限时活动任务（如 WorkBuddy_Valorant「体验无畏契约拼豆专家」，
// 有效期至 2026-11-15）。活动下线后脚本侧按 valid_end 整体跳过、正常退出，
// 因此排程无需感知具体活动的存亡——新增限时活动只需在 MAPPING 加 valid_end。
//
// 全量 growth 补跑会自动排除限时任务（脚本内 LIMITED_CODES），两个排程不会
// 对同一任务重复点亮。共享 taskRunnerMu：两者是同一脚本、同一批 auth 文件。
func (s *Scheduler) RunLimitedTasksNow() { s.runLimitedTasksNow(context.Background()) }

func (s *Scheduler) runLimitedTasksNow(ctx context.Context) {
	taskRunnerMu.Lock()
	defer taskRunnerMu.Unlock()
	root := repoRoot()
	runScriptContext(ctx, "limited", root, [][]string{
		s.accountScriptArgs(ctx, []string{pythonCmd(), "scripts/task_runner.py", "ALL", "--yes", "--limited"}),
	})
}
