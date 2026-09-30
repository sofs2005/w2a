// aliasfile.go 读写 workbuddy2api 网关的模型别名映射文件（model_aliases.json）。
//
// 与 config.json 的关键差别：**这份文件热生效**。网关每 5s 轮询它的签名
// （cmd/server/reload.go 的 startAliasWatcher），面板保存后数秒内新映射就进调度，
// 不需要重启容器、不需要 docker.sock。因此本文件的写路径必须比 config.json 更稳：
// 写坏的瞬间线上正在跑的映射会被"保持旧表"逻辑挡住（网关侧不认非法 JSON），
// 但面板仍应尽最大努力不写出半截文件——沿用 fsutil.WriteFileAtomic + 首次备份。
package ops

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"workbuddy2api-gui/internal/fsutil"
)

// AliasEntry 单条别名映射：对外名 → 各域真实上游模型名。
//
// CN / Global 可各留空其一（该域不存在此模型），但不可同时为空——网关侧
// internal/aliases.Parse 会整体拒绝这类条目。面板在写入前先做同样校验，
// 让用户当场看到错误，而不是保存后才发现网关"保持旧表"没生效。
type AliasEntry struct {
	Name   string `json:"name"`
	CN     string `json:"cn"`
	Global string `json:"global"`
}

// AliasFile 磁盘格式（与网关 internal/aliases.File 同构，字段名必须一致）。
type AliasFile struct {
	Aliases []AliasEntry `json:"aliases"`
}

// AliasMeta 别名文件元信息。
type AliasMeta struct {
	Path       string    `json:"path"`
	Exists     bool      `json:"exists"`
	Size       int64     `json:"size"`
	ModTime    time.Time `json:"mod_time,omitempty"`
	BackupPath string    `json:"backup_path,omitempty"`
	BackupAt   time.Time `json:"backup_at,omitempty"`
	ParseError string    `json:"parse_error,omitempty"`
	// RestartNote 与 config.json 的提示语刻意不同：本文件不需要重启。
	RestartNote string `json:"restart_note"`
}

// aliasFileName 别名文件名（网关侧 aliasFilePath 推导出的固定名，必须一致）。
const aliasFileName = "model_aliases.json"

// AliasFilePath 返回别名文件的绝对路径。
//
// 两级取值：
//  1. 面板配置显式指定（config_file 之外的 alias_file）——特殊布局用；
//  2. 否则从网关 config.json 的 state_file 推导，规则与网关 cmd/server/main.go 的
//     aliasFilePath 完全一致：同目录下的 model_aliases.json。
//
// 为什么默认走推导而不是在面板配置里再填一份：两边**必须算出同一个路径**，否则面板
// 保存成功但网关读的是另一个文件（表现为"改了没反应"，且极难排查）。让唯一事实来源
// 落在网关自己的 config.json 上，就消除了这种不一致的可能。
//
// state_file 为空/缺失（网关用纯内存状态）→ 返回空串 = 本功能不可用（网关侧同样不会
// 去读别名文件，因为推导不出路径）。
func (s *Service) AliasFilePath() string {
	if p := strings.TrimSpace(s.cfg.UpstreamAliasFile); p != "" {
		return p
	}
	cfg, _, err := s.ReadUpstreamConfig()
	if err != nil || cfg == nil {
		return ""
	}
	stateFile, _ := cfg["state_file"].(string)
	if stateFile == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(stateFile), aliasFileName)
}

// ReadAliases 读取别名文件，返回条目与元信息。
//
// 文件不存在不报错：返回空列表 + Exists=false，让前端从空白表开始编辑
// （默认部署没有别名文件，这是正常状态而非错误）。
func (s *Service) ReadAliases() ([]AliasEntry, *AliasMeta, error) {
	path := s.AliasFilePath()
	if path == "" {
		return nil, nil, fmt.Errorf("无法推导别名文件路径：网关 config.json 未配置 state_file" +
			"（别名文件与池状态文件同目录，见部署说明）")
	}
	meta := &AliasMeta{
		Path:        path,
		RestartNote: "保存后网关数秒内自动热加载，无需重启",
	}
	st, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return []AliasEntry{}, meta, nil
		}
		return nil, meta, fmt.Errorf("读取别名文件失败: %w", err)
	}
	meta.Exists = true
	meta.Size = st.Size()
	meta.ModTime = st.ModTime()
	if bp, bt := s.backupInfo(path); bp != "" {
		meta.BackupPath = bp
		meta.BackupAt = bt
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, meta, fmt.Errorf("读取别名文件失败: %w", err)
	}
	var f AliasFile
	if len(strings.TrimSpace(string(raw))) > 0 {
		if err := json.Unmarshal(raw, &f); err != nil {
			meta.ParseError = err.Error()
			return nil, meta, fmt.Errorf("别名文件不是合法 JSON: %w", err)
		}
	}
	if f.Aliases == nil {
		f.Aliases = []AliasEntry{}
	}
	return f.Aliases, meta, nil
}

// WriteAliases 写入别名文件（原子替换 + 首次备份），并在写入前做与网关同口径的校验。
//
// 为什么要在此校验：网关解析失败时是**静默保持旧表 + WARN**，用户从面板上看不出
// 自己刚保存的东西没生效。在面板这侧提前拦下来，错误信息能直接指到第几行。
func (s *Service) WriteAliases(entries []AliasEntry) (fallback bool, err error) {
	if err := s.ensureWritable(); err != nil {
		return false, err
	}
	path := s.AliasFilePath()
	if path == "" {
		return false, fmt.Errorf("无法推导别名文件路径：网关 config.json 未配置 state_file")
	}
	norm, err := normalizeAliases(entries)
	if err != nil {
		return false, err
	}
	raw, err := json.MarshalIndent(AliasFile{Aliases: norm}, "", "  ")
	if err != nil {
		return false, fmt.Errorf("别名表无法序列化: %w", err)
	}
	raw = append(raw, '\n')

	if err := s.ensureBackup(path); err != nil {
		return false, err
	}
	return fsutil.WriteFileAtomic(path, raw, 0o600)
}

// ResetAliases 从备份恢复别名文件（高危：需 dangerous_ops）。
func (s *Service) ResetAliases() error {
	if err := s.ensureDangerous(); err != nil {
		return err
	}
	path := s.AliasFilePath()
	if path == "" {
		return fmt.Errorf("无法推导别名文件路径：网关 config.json 未配置 state_file")
	}
	backup, _ := s.backupInfo(path)
	if backup == "" {
		return fmt.Errorf("没有可用的备份文件（保存过一次别名后才会生成）")
	}
	raw, err := os.ReadFile(backup)
	if err != nil {
		return err
	}
	var probe AliasFile
	if err := json.Unmarshal(raw, &probe); err != nil {
		return fmt.Errorf("备份文件不是合法 JSON，拒绝恢复: %w", err)
	}
	if _, err := fsutil.WriteFileAtomic(path, raw, 0o600); err != nil {
		return err
	}
	return nil
}

// normalizeAliases 校验并规范化条目（去空白、拒重复、拒两域同空）。
//
// 校验口径与网关 internal/aliases.Parse 一致，唯一的差别是错误文案面向面板用户
// （带行号，且解释"为什么"）。
func normalizeAliases(entries []AliasEntry) ([]AliasEntry, error) {
	out := make([]AliasEntry, 0, len(entries))
	seen := map[string]bool{}
	for i, e := range entries {
		name := strings.TrimSpace(e.Name)
		cn := strings.TrimSpace(e.CN)
		gl := strings.TrimSpace(e.Global)
		row := i + 1
		if name == "" {
			return nil, fmt.Errorf("第 %d 行：对外模型名不能为空", row)
		}
		if strings.Contains(name, ":") {
			return nil, fmt.Errorf("第 %d 行：对外模型名 %q 不能含冒号——"+
				"\"cn:\"/\"global:\" 是网关的路由前缀，与之冲突的别名永远不会被查到", row, name)
		}
		if cn == "" && gl == "" {
			return nil, fmt.Errorf("第 %d 行：%q 的 CN 名与 global 名不能同时为空"+
				"（至少填一个，否则没有任何域能出站）", row, name)
		}
		if seen[name] {
			return nil, fmt.Errorf("第 %d 行：对外模型名 %q 重复", row, name)
		}
		seen[name] = true
		out = append(out, AliasEntry{Name: name, CN: cn, Global: gl})
	}
	return out, nil
}
