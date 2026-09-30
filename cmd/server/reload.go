// reload.go 账号目录热加载：替代「重启网关容器」以加载新账号。
//
// 为什么需要：上游只在启动时调一次 auth.LoadDir + pool.SyncToDir（见 main.go），
// 此后新增的凭证文件不会被扫到。原先面板靠 `docker restart` 解决，但合并成单进程后
// 重启等于重启自己（面板随之断线，且要挂 docker.sock 交出宿主机 Docker 控制权）。
//
// 改为进程内重扫后：面板扫码落盘 → 数秒内自动进池，无需重启、零停机，也不需要
// docker.sock。同时覆盖「手工投放凭证文件」与「删除凭证文件」两种运维路径。
package main

import (
	"context"
	"log"
	"os"
	"sort"
	"strings"
	"time"

	"workbuddy2api/internal/aliases"
	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
)

// authWatchInterval 目录轮询周期。
//
// 5s 是「用户感知为即时」与「文件系统开销可忽略」的折中：一次轮询只是 ReadDir
// 加文件名比对，没有解析开销（仅当集合变化才真正 LoadDir）。
const authWatchInterval = 5 * time.Second

// aliasWatchInterval 别名文件轮询周期（与 authWatchInterval 同值同理由：一次轮询只是
// Stat + 签名比对，只有内容真变了才解析 JSON）。
const aliasWatchInterval = 5 * time.Second

// authDirSignature 计算凭证目录的「文件集合 + 大小 + 修改时间」指纹。
//
// 只比文件名集合不够：网关自身会回写凭证（token 刷新、realm backfill 走
// tmp+rename），内容变化而文件名不变时也应重载，否则内存里的 token 会落后于磁盘。
// 返回空串表示目录不可读（此时不触发重载，避免把账号全清空）。
func authDirSignature(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	parts := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		// 只认 auth.LoadAuthFiles 会处理的形态（AuthFileGlob = "workbuddy*.json"），
		// 避免被无关文件（.tmp 中间态、备份、编辑器临时文件）反复触发。
		if !strings.HasSuffix(name, ".json") || !strings.HasPrefix(name, "workbuddy") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		parts = append(parts, name+"|"+
			time.Duration(info.Size()).String()+"|"+
			info.ModTime().UTC().Format(time.RFC3339Nano))
	}
	sort.Strings(parts)
	return strings.Join(parts, "\n")
}

// startAuthWatcher 启动后台账号目录监听，返回停止函数。
//
// 首次调用记录基线指纹，之后每次变化才重扫。重扫是幂等的：pool.upsertLocked
// 明确保留已有账号的运行时状态（credits / 冷却 / 计数），故反复 SyncToDir
// 不会打乱调度，只做「新增纳入、删除剔除、凭证更新」。
func startAuthWatcher(ctx context.Context, authDir string, p *pool.Pool) func() {
	stop := make(chan struct{})
	last := authDirSignature(authDir)

	go func() {
		t := time.NewTicker(authWatchInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-stop:
				return
			case <-t.C:
			}

			sig := authDirSignature(authDir)
			if sig == "" {
				// 目录短暂不可读（挂载抖动 / 权限）：保持现状，下轮再试。
				// 直接当作「空目录」会把账号全踢出池，代价远大于多等 5s。
				continue
			}
			if sig == last {
				continue
			}

			auths, err := auth.LoadDir(authDir)
			if err != nil {
				log.Printf("WARN: [panel] 账号热加载失败（保持现有池）: %v", err)
				continue
			}
			before, _, _, _, _ := p.CountsDetailed()
			p.SyncToDir(auths)
			after, _, _, _, _ := p.CountsDetailed()
			last = sig

			// 日志如实反映结果：
			//   · 池大小变化 → 新增/剔除
			//   · 池大小不变但磁盘文件数 > 池大小 → 有文件读不进来（多半是属主/权限，
			//     例如宿主机以 uid 1000 落盘、容器以 10001 读），必须点出来，
			//     否则用户会以为账号已加载而实际没有（auth.LoadDir 对不可读文件是静默跳过）。
			//   · 其余 → 既有账号的凭证内容更新（token 刷新 / realm 回填）
			switch {
			case before != after:
				log.Printf("账号目录变化：%d → %d 个账号（已热加载，无需重启）", before, after)
			case len(auths) > after:
				log.Printf("WARN: 账号目录有 %d 个凭证文件，但仅 %d 个载入池中——"+
					"多半是文件属主/权限问题（网关以 uid %d 运行）。"+
					"宿主机执行：chown -R 10001:10001 ./auths",
					len(auths), after, os.Getuid())
			default:
				log.Printf("账号目录变化：%d 个账号凭证已更新（已热加载）", after)
			}
		}
	}()

	return func() { close(stop) }
}

// startAliasWatcher 启动别名文件监听，返回停止函数（与 startAuthWatcher 同模式）。
//
// 为什么需要：别名映射是**高频调整**的运维参数（"这个模型两域名不一样"、"隐藏模型
// 只在一域有"），而网关只在启动时读 config.json——放 config 就得每次重启。独立文件 +
// 5s 轮询让面板改完数秒内生效，与账号目录热加载的体验一致。
//
// 失败语义（关键）：签名读不到（文件不存在 → "missing"）或解析失败时**保持旧表**
// 并打 WARN，绝不把别名清空。面板写坏文件、编辑器保存到一半、权限抖动等都不该
// 影响线上正在跑的映射。仅当解析成功才发布新快照。
//
// 首次调用发布基线（store 由调用方在启动时先 Load 一次，此处只对齐指纹，避免重复
// 加载；若调用方未加载，本函数首轮签名变化时也会兜底加载）。
func startAliasWatcher(ctx context.Context, path string, store *aliases.Store) func() {
	stop := make(chan struct{})
	last := aliases.Signature(path)

	go func() {
		t := time.NewTicker(aliasWatchInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-stop:
				return
			case <-t.C:
			}

			last = reloadAliasesOnce(path, store, last)
		}
	}()

	return func() { close(stop) }
}

// reloadAliasesOnce 执行一轮别名热加载，返回新的基线指纹。
//
// 从轮询 goroutine 里提出来是为了可测：等待 5s ticker 的测试既慢又脆，
// 而"签名变了才解析、解析失败保持旧表"这条契约正是本功能的全部要害。
//
// 返回旧指纹的两种情况（都表示"这次没生效"）：
//   - 签名未变（无操作）；
//   - 解析失败（保持旧表 + WARN，**不推进基线**——文件修好后签名会再变，届时正常重载；
//     若在此推进基线，坏文件修好后若大小/mtime 恰好回到旧值就会被漏掉）。
func reloadAliasesOnce(path string, store *aliases.Store, last string) string {
	sig := aliases.Signature(path)
	if sig == last {
		return last
	}
	tbl, err := aliases.Load(path)
	if err != nil {
		// 非法 JSON / 校验不过：保持旧表（不清空），下轮再试。
		log.Printf("WARN: [aliases] 别名热加载失败（保持现有映射）: %v", err)
		return last
	}
	store.Set(tbl)
	log.Printf("别名映射已热加载：%d 条（无需重启）", tbl.Len())
	return sig
}
