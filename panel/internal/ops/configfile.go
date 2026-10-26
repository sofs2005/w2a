// configfile.go 读写 workbuddy2api 网关的 config.json。
//
// 安全取态：网关自身不提供配置读写接口，GUI 直接改磁盘文件。因此必须做到
// 「写入前先备份 + 先校验 JSON + 原子替换」，避免一次误输入把网关配置写坏导致起不来。
package ops

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"workbuddy2api-gui/internal/fsutil"
)

// UpstreamConfigMeta 配置文件元信息。
type UpstreamConfigMeta struct {
	Path        string    `json:"path"`
	Exists      bool      `json:"exists"`
	Size        int64     `json:"size"`
	ModTime     time.Time `json:"mod_time,omitempty"`
	BackupPath  string    `json:"backup_path,omitempty"`
	BackupAt    time.Time `json:"backup_at,omitempty"`
	ParseError  string    `json:"parse_error,omitempty"`
	IsValidJSON bool      `json:"is_valid_json"`
	RestartNote string    `json:"restart_note"`
	// Backups 现存可恢复的备份（最新在前，最多 backupKeep 份）。
	Backups []BackupEntry `json:"backups,omitempty"`
}

// ReadUpstreamConfig 读取网关 config.json，返回解析后的对象与元信息。
// 文件不存在时不报错：返回空对象 + Exists=false，让前端从默认模板开始编辑。
func (s *Service) ReadUpstreamConfig() (map[string]any, *UpstreamConfigMeta, error) {
	path := s.cfg.UpstreamConfigFile
	if path == "" {
		return nil, nil, fmt.Errorf("未配置网关配置文件路径（config_file）")
	}
	meta := &UpstreamConfigMeta{
		Path:        path,
		RestartNote: "保存后立即热生效（面板直调网关重载）；只有 listen / auth_dir / state_file / server / upstash 改动仍需重启进程。手工改文件不触发热重载",
	}
	st, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]any{}, meta, nil
		}
		return nil, meta, fmt.Errorf("读取配置失败: %w", err)
	}
	meta.Exists = true
	meta.Size = st.Size()
	meta.ModTime = st.ModTime()

	// 备份文件信息（若存在）。
	if bp, bt := s.backupInfo(path); bp != "" {
		meta.BackupPath = bp
		meta.BackupAt = bt
	}
	meta.Backups = s.listBackups(path)

	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, meta, fmt.Errorf("读取配置失败: %w", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		meta.ParseError = err.Error()
		return nil, meta, fmt.Errorf("config.json 不是合法 JSON: %w", err)
	}
	meta.IsValidJSON = true
	return doc, meta, nil
}

// WriteUpstreamConfig 写入网关 config.json。
//
// 流程：JSON 序列化 → 把当前内容存成最新一份滚动备份 → 原子替换（单文件 bind mount
// 下自动回退原地写）。任何一步失败都不会让原文件处于半截状态（见 fsutil.WriteFileAtomic
// 的说明）；备份轮转见 backup.go（保留最近 backupKeep 份，可任选一份恢复）。
func (s *Service) WriteUpstreamConfig(doc map[string]any) error {
	_, err := s.WriteUpstreamConfigDetailed(doc)
	return err
}

// WriteUpstreamConfigDetailed 同 WriteUpstreamConfig，但额外告知是否走了
// 「单文件挂载回退路径」——回退写不是原子的，值得在响应里提示用户。
func (s *Service) WriteUpstreamConfigDetailed(doc map[string]any) (fallback bool, err error) {
	if err := s.ensureWritable(); err != nil {
		return false, err
	}
	path := s.cfg.UpstreamConfigFile
	if path == "" {
		return false, fmt.Errorf("未配置网关配置文件路径（config_file）")
	}
	// 先做一次校验性序列化，确保对象本身可编码。
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return false, fmt.Errorf("配置无法序列化: %w", err)
	}
	raw = append(raw, '\n')

	// 写入前把当前内容存成最新备份（保留最近 backupKeep 份，见 backup.go）。
	if err := s.rotateBackup(path); err != nil {
		return false, err
	}

	return fsutil.WriteFileAtomic(path, raw, 0o600)
}

// backupSuffix 备份文件后缀。备份文件的命名与轮转见 backup.go。
const backupSuffix = ".gui.bak"

// ResetUpstreamConfig 从备份恢复网关 config.json（高危：需 dangerous_ops）。
//
// backupIndex <= 0 表示最新一份（= 上一次保存前的内容）；也可指定更早的一份。
// 恢复前会先把当前内容存为新备份，所以恢复本身也能被撤销。
func (s *Service) ResetUpstreamConfig(backupIndex int) (BackupEntry, error) {
	if err := s.ensureDangerous(); err != nil {
		return BackupEntry{}, err
	}
	path := s.cfg.UpstreamConfigFile
	entry, err := s.pickBackup(path, backupIndex)
	if err != nil {
		return BackupEntry{}, err
	}
	raw, err := os.ReadFile(entry.Path)
	if err != nil {
		return BackupEntry{}, err
	}
	var probe map[string]any
	if err := json.Unmarshal(raw, &probe); err != nil {
		return BackupEntry{}, fmt.Errorf("备份文件不是合法 JSON，拒绝恢复: %w", err)
	}
	// 先给当前内容留一份备份再覆盖：恢复是最容易点错的操作，误恢复必须能撤销。
	// 顺序不能反——先写再备份，存下来的就是刚恢复的那份，等于没有回退点。
	if err := s.rotateBackup(path); err != nil {
		return BackupEntry{}, err
	}
	if _, err := fsutil.WriteFileAtomic(path, raw, 0o600); err != nil {
		return BackupEntry{}, err
	}
	return entry, nil
}
