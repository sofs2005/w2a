package ops

import (
	"os"
	"path/filepath"
	"testing"

	"workbuddy2api-gui/internal/config"
)

// newBackupService 构造一个把网关配置指向临时文件的 Service。
// 备份目录指向临时目录下的子目录，避免与宿主机的 ./data 混淆。
func newBackupService(t *testing.T) (*Service, string) {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"n":0}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.UpstreamConfigFile = cfgPath
	cfg.BackupDir = filepath.Join(dir, "backups")
	cfg.DangerousOps = true
	return New(cfg, nil, nil, nil), cfgPath
}

// readFile 读文件内容，失败即 Fatal。
func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s: %v", path, err)
	}
	return string(b)
}

// TestBackupRotatesAndKeepsLatest 备份必须随每次保存滚动，且只保留最近 3 份。
//
// 这是本次修复的核心：旧实现只在首次保存前备份一次，之后「恢复备份」永远回到
// 最初那份，回退点形同废纸。
func TestBackupRotatesAndKeepsLatest(t *testing.T) {
	s, cfgPath := newBackupService(t)

	// 连续保存 5 次（内容 1..5）。每次保存前都会把上一次的内容存成备份。
	for i := 1; i <= 5; i++ {
		doc := map[string]any{"n": float64(i)}
		if _, err := s.WriteUpstreamConfigDetailed(doc); err != nil {
			t.Fatalf("第 %d 次保存: %v", i, err)
		}
	}

	list := s.listBackups(cfgPath)
	if len(list) != backupKeep {
		t.Fatalf("备份份数=%d want %d", len(list), backupKeep)
	}
	// 最新一份 = 第 4 次保存后的内容（即第 5 次保存前的内容）。
	if got := readFile(t, list[0].Path); got != `{
  "n": 4
}
` {
		t.Errorf("最新备份内容=%q want n=4", got)
	}
	// 最旧一份 = 第 2 次保存后的内容：1..5 次保存共产生 5 份备份（n=0..4），
	// 只留最近 3 份 → n=2,3,4。
	if got := readFile(t, list[backupKeep-1].Path); got != `{
  "n": 2
}
` {
		t.Errorf("最旧备份内容=%q want n=2", got)
	}
}

// TestRestorePicksRequestedBackup 可以恢复到指定序号（1 = 上一次保存前）。
//
// 注意：恢复本身会先把当前内容存成新备份，所以序号在恢复后会整体后移一位。
func TestRestorePicksRequestedBackup(t *testing.T) {
	s, cfgPath := newBackupService(t)
	for i := 1; i <= 3; i++ {
		if _, err := s.WriteUpstreamConfigDetailed(map[string]any{"n": float64(i)}); err != nil {
			t.Fatalf("第 %d 次保存: %v", i, err)
		}
	}
	// 此时备份：1 → n=2（上一次），2 → n=1，3 → n=0。

	entry, err := s.ResetUpstreamConfig(1)
	if err != nil {
		t.Fatalf("恢复到最新备份: %v", err)
	}
	if entry.Index != 1 {
		t.Errorf("恢复的备份序号=%d want 1", entry.Index)
	}
	if got := readFile(t, cfgPath); got != `{
  "n": 2
}
` {
		t.Errorf("恢复后内容=%q want n=2", got)
	}

	// 恢复本身留了一份备份，序号整体后移一位。这里按「内容」而非序号定位，
	// 避免把轮转细节写死在断言里（且必须在恢复前读，恢复会再次轮转覆盖该文件）。
	target, err := s.pickBackup(cfgPath, 3)
	if err != nil {
		t.Fatalf("第 3 份备份: %v", err)
	}
	want := readFile(t, target.Path)
	if _, err := s.ResetUpstreamConfig(3); err != nil {
		t.Fatalf("恢复第 3 份: %v", err)
	}
	if got := readFile(t, cfgPath); got != want {
		t.Errorf("恢复第 3 份后内容=%q want %q", got, want)
	}
}

// TestRestoreIsUndoable 恢复本身也要留备份：误恢复后还能回到恢复前的内容。
func TestRestoreIsUndoable(t *testing.T) {
	s, cfgPath := newBackupService(t)
	for i := 1; i <= 2; i++ {
		if _, err := s.WriteUpstreamConfigDetailed(map[string]any{"n": float64(i)}); err != nil {
			t.Fatalf("第 %d 次保存: %v", i, err)
		}
	}
	// 当前文件 n=2；备份 1 → n=1，2 → 原始文件（n=0）。
	before := readFile(t, cfgPath)
	target, err := s.pickBackup(cfgPath, 2)
	if err != nil {
		t.Fatal(err)
	}
	want := readFile(t, target.Path)

	if _, err := s.ResetUpstreamConfig(2); err != nil {
		t.Fatalf("恢复: %v", err)
	}
	// 恢复写入的是备份文件的**原始字节**（不做重新格式化），故与备份逐字节一致。
	if got := readFile(t, cfgPath); got != want {
		t.Fatalf("恢复后内容=%q want %q", got, want)
	}

	// 恢复前的 n=2 现在是最新备份：再恢复一次应回到它。
	if _, err := s.ResetUpstreamConfig(1); err != nil {
		t.Fatalf("撤销恢复: %v", err)
	}
	if got := readFile(t, cfgPath); got != before {
		t.Errorf("撤销恢复后内容=%q want %q", got, before)
	}
}

// TestBackupMissingIndex 指定不存在的备份要明确报错，而不是静默恢复成别的。
func TestBackupMissingIndex(t *testing.T) {
	s, _ := newBackupService(t)
	if _, err := s.WriteUpstreamConfigDetailed(map[string]any{"n": float64(1)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResetUpstreamConfig(3); err == nil {
		t.Error("恢复不存在的备份应报错")
	}
}

// TestBackupAdoptsLegacyFile 老版本留下的无序号备份不能被丢掉，应作为最旧一份纳入。
func TestBackupAdoptsLegacyFile(t *testing.T) {
	s, cfgPath := newBackupService(t)
	dir := s.backupDirFor(cfgPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(dir, backupBase(cfgPath))
	if err := os.WriteFile(legacy, []byte(`{"legacy":true}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := s.WriteUpstreamConfigDetailed(map[string]any{"n": float64(1)}); err != nil {
		t.Fatal(err)
	}

	list := s.listBackups(cfgPath)
	if len(list) != 2 {
		t.Fatalf("备份份数=%d want 2（新一份 + 老文件）", len(list))
	}
	// 最新一份是刚才保存前的内容（带缩进的原文件写法），老文件被挪到第 2 位。
	if got := readFile(t, list[0].Path); got != `{"n":0}` {
		t.Errorf("最新备份内容=%q want 原文件内容", got)
	}
	if got := readFile(t, list[1].Path); got != `{"legacy":true}` {
		t.Errorf("老备份未被保留: %q", got)
	}
}

// TestBackupFirstWriteNoop 目标文件尚不存在时不产生备份（没有「上一次」可存）。
func TestBackupFirstWriteNoop(t *testing.T) {
	s, cfgPath := newBackupService(t)
	if err := os.Remove(cfgPath); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WriteUpstreamConfigDetailed(map[string]any{"n": float64(1)}); err != nil {
		t.Fatal(err)
	}
	if list := s.listBackups(cfgPath); len(list) != 0 {
		t.Errorf("首次创建不应产生备份: %v", list)
	}
}
