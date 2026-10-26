// backup.go 受管文件的滚动备份（config.json / model_aliases.json 共用）。
//
// 旧行为是「首次保存前备份一份，之后永不更新」，于是「恢复备份」永远回到最初那份
// ——改了几十次之后，那个回退点早就没有意义了。现在改为**每次写入前**把当前内容
// 存为新备份，并只保留最近 backupKeep 份（最新的在前），恢复时可选其中任意一份。
//
// 文件名带递增序号而不是时间戳：序号天然保证「最新的一份最大」，不依赖时钟
// （容器时间漂移、同一秒内连按两次保存都会让时间戳方案排序出错）。
//
//	config.json.gui.bak.1   ← 最近一次保存前的内容（最新的备份）
//	config.json.gui.bak.2
//	config.json.gui.bak.3   ← 保留的最旧一份
//
// 注意序号语义：**不是**「第 1 次保存」。每保存一次就整体右移，.1 永远是「上一次」。
package ops

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"workbuddy2api-gui/internal/fsutil"
)

// backupKeep 每个受管文件保留的备份份数（最新 backupKeep 份）。
const backupKeep = 3

// BackupEntry 一份可恢复的备份。
type BackupEntry struct {
	// Path 备份文件的绝对/实际路径。
	Path string `json:"path"`
	// At 备份时间（文件修改时间）。
	At time.Time `json:"at"`
	// Size 备份内容大小。
	Size int64 `json:"size"`
	// Index 1 = 最近一次保存前的内容，2 = 更早一份，依此类推。
	Index int `json:"index"`
}

// backupDirFor 返回该文件的备份目录。
//
// 为什么备份位置要特别处理：Docker 部署时 config.json 常以**单文件**方式挂载，
// 此时容器内 /gateway 目录是容器自己的可写层，而不是宿主机目录 —— 备份写在那里
// 会随容器重建而消失（用户以为有回退点，实际没有）。因此：
//   - 若配置文件所在目录本身是挂载进来的（目录可写且与文件同设备），直接写旁边；
//   - 否则回退到面板自己的数据目录（配置里的 backup_dir / 默认 ./data），
//     并在元信息里把真实路径告诉前端。
func (s *Service) backupDirFor(path string) string {
	if sameDevice(path, filepath.Dir(path)) {
		return filepath.Dir(path)
	}
	dir := s.cfg.BackupDir
	if dir == "" {
		dir = "./data/backups"
	}
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	return dir
}

// backupBase 返回某文件的备份文件名前缀（目录另算）。
func backupBase(path string) string {
	return filepath.Base(path) + backupSuffix
}

// backupPath 返回第 idx 份备份的路径（idx 从 1 开始）。
func backupPath(dir, base string, idx int) string {
	return filepath.Join(dir, fmt.Sprintf("%s.%d", base, idx))
}

// rotateBackup 在**写入目标文件之前**调用：把目标当前内容存为最新备份，
// 其余备份依次后移，超出 backupKeep 的最旧一份删除。
//
// 目标文件不存在（首次创建）时是 no-op：没有「上一次内容」可备份。
func (s *Service) rotateBackup(path string) error {
	orig, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // 首次创建，无需备份
		}
		return fmt.Errorf("读取原文件失败（已中止，未改动原文件）: %w", err)
	}

	dir := s.backupDirFor(path)
	base := backupBase(path)
	if dir != filepath.Dir(path) {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("创建备份目录失败: %w", err)
		}
	}

	// 从最旧一份开始往后挪，避免覆盖掉还没搬走的下一份。
	// 旧版本只有无序号的一份（<base>），先把它当作「最旧」纳入轮转，避免丢历史。
	legacy := filepath.Join(dir, base)
	for i := backupKeep; i >= 2; i-- {
		from := backupPath(dir, base, i-1)
		if i-1 == 1 {
			// 第 1 份的上一代可能是老版本留下的无序号文件。
			if _, err := os.Stat(from); os.IsNotExist(err) {
				from = legacy
			}
		}
		if _, err := os.Stat(from); err != nil {
			continue // 该代还不存在，跳过
		}
		to := backupPath(dir, base, i)
		_ = os.Remove(to) // 目标位置即将被覆盖（最旧一份在这里被丢弃）
		if err := os.Rename(from, to); err != nil {
			return fmt.Errorf("轮转备份失败: %w", err)
		}
	}

	// 最新一份直接写内容，不用 rename：orig 已在内存里，避免依赖目标文件的 inode。
	if err := os.WriteFile(backupPath(dir, base, 1), orig, 0o600); err != nil {
		return fmt.Errorf("写入备份失败（已中止，未改动原文件）: %w", err)
	}
	return nil
}

// listBackups 返回该文件现存的可恢复备份，最新的在前。
//
// 兼容老版本：无序号的那份（<base>）作为「最旧」附在末尾，保证升级后旧回退点仍可用。
func (s *Service) listBackups(path string) []BackupEntry {
	dir := s.backupDirFor(path)
	base := backupBase(path)
	var out []BackupEntry
	for i := 1; i <= backupKeep; i++ {
		p := backupPath(dir, base, i)
		st, err := os.Stat(p)
		if err != nil {
			continue
		}
		out = append(out, BackupEntry{Path: p, At: st.ModTime(), Size: st.Size(), Index: i})
	}
	if legacy := filepath.Join(dir, base); len(out) < backupKeep {
		if st, err := os.Stat(legacy); err == nil {
			out = append(out, BackupEntry{Path: legacy, At: st.ModTime(), Size: st.Size(), Index: len(out) + 1})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Index < out[j].Index })
	return out
}

// backupInfo 返回最新一份备份的路径与时间（供元信息展示）；没有则返回零值。
func (s *Service) backupInfo(path string) (string, time.Time) {
	if list := s.listBackups(path); len(list) > 0 {
		return list[0].Path, list[0].At
	}
	return "", time.Time{}
}

// pickBackup 按 index 选一份备份；index <= 0 表示「最新一份」。
func (s *Service) pickBackup(path string, index int) (BackupEntry, error) {
	list := s.listBackups(path)
	if len(list) == 0 {
		return BackupEntry{}, fmt.Errorf("没有可用的备份文件（保存过一次配置后才会生成）")
	}
	if index <= 0 {
		return list[0], nil
	}
	for _, b := range list {
		if b.Index == index {
			return b, nil
		}
	}
	return BackupEntry{}, fmt.Errorf("备份 %d 不存在（当前只有 %d 份）", index, len(list))
}

// restoreBackup 把指定备份写回目标文件（原子替换），并先备份当前内容。
//
// 恢复本身也是一次写入：若不留一份「恢复前」的备份，误恢复就没有回头路，
// 而恢复恰恰是最容易点错的操作。
func (s *Service) restoreBackup(path string, index int) (BackupEntry, error) {
	entry, err := s.pickBackup(path, index)
	if err != nil {
		return BackupEntry{}, err
	}
	raw, err := os.ReadFile(entry.Path)
	if err != nil {
		return BackupEntry{}, err
	}
	if err := s.rotateBackup(path); err != nil {
		return BackupEntry{}, err
	}
	if _, err := fsutil.WriteFileAtomic(path, raw, 0o600); err != nil {
		return BackupEntry{}, err
	}
	return entry, nil
}

// sameDevice 报告 path 与其所在目录是否在同一文件系统上。
// 不同设备号 = 单文件 bind mount（或跨设备挂载），此时目录不反映宿主机目录。
func sameDevice(path, dir string) bool {
	fi, err := os.Stat(path)
	if err != nil {
		return false
	}
	di, err := os.Stat(dir)
	if err != nil {
		return false
	}
	fsDev, ok1 := fsutil.DeviceID(fi)
	dsDev, ok2 := fsutil.DeviceID(di)
	if !ok1 || !ok2 {
		// 非 Unix：无法判断设备，按"同目录可写"处理。
		return true
	}
	return fsDev == dsDev
}
