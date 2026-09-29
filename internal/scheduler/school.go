// school.go 开学季任务、夜猫子任务与成长任务补跑的脚本类排程：从系统 crontab 迁入 Go scheduler。
//
// 背景：school（12:00）与 cat（01:00 夜猫窗口）原由系统 crontab 调
// scripts/school_open_day_cron.sh 执行——依赖外部系统 cron、容器重建可能丢失、
// 不在 config 里配置。迁入后成为第五、第六类任务，时点由 schedule.school_hours /
// schedule.cat_hours 配置，school_open_day_cron.sh 保留为手动触发入口。
// 第七类 growth（08:00）是后来补的：同脚本 task_runner.py 的全量入口，见 RunGrowthTasksNow。
package scheduler

import (
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// repoRoot 定位仓库根（容器内 /app、宿主 /root/workbuddy2api）。
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

// scriptCmd exec.Cmd 适配器：把 exec.Cmd 的 Dir 字段包装成 SetDir 方法，
// 满足 scriptRunner 接口（exec.Cmd 本身只有字段没有方法）。
type scriptCmd struct{ cmd *exec.Cmd }

func (c *scriptCmd) SetDir(dir string) { c.cmd.Dir = dir }
func (c *scriptCmd) Run() error        { return c.cmd.Run() }

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
	for _, cmdArgs := range commands {
		c := newScriptCmd(cmdArgs[0], cmdArgs[1:]...)
		c.SetDir(root)
		if err := c.Run(); err != nil {
			log.Printf("WARN: %s (%s): %v", name, cmdArgs[1], err)
			continue
		}
		log.Printf("%s: ok (%s)", name, cmdArgs[1])
	}
}

// RunSchoolNow 立即执行开学季任务：school_open_day_2026.py ALL --run --yes。
// 全量跑任务点亮 + 领奖 + 自动抽空抽奖余额。活动下线（in_period=false）时脚本
// 各段全量跳过、正常退出，不视为失败。失败只记 WARN。
func (s *Scheduler) RunSchoolNow() {
	root := repoRoot()
	runScript("school", root, [][]string{
		{pythonCmd(), "scripts/school_open_day_2026.py", "ALL", "--run", "--yes"},
	})
}

// RunCatNow 立即执行夜猫子任务：task_runner.py ALL --yes --only black_cat。
// black_cat 时段敏感：夜猫窗口 23:00–08:00 CST 内最多补 1 次（task_runner 内部
// 判定，非窗口期打印 skip 正常退出）。失败只记 WARN。
func (s *Scheduler) RunCatNow() {
	taskRunnerMu.Lock()
	defer taskRunnerMu.Unlock()
	root := repoRoot()
	runScript("cat", root, [][]string{
		{pythonCmd(), "scripts/task_runner.py", "ALL", "--yes", "--only", "black_cat"},
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
func (s *Scheduler) RunGrowthTasksNow() {
	taskRunnerMu.Lock()
	defer taskRunnerMu.Unlock()
	root := repoRoot()
	runScript("growth", root, [][]string{
		{pythonCmd(), "scripts/task_runner.py", "ALL", "--yes"},
	})
}
