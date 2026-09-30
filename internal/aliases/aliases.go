// Package aliases 模型别名映射表：把「统一对外模型名」映射到各域的真实上游模型名。
//
// 为什么需要：CN 与 global 是同一套 API 的两次部署，同 id 模型大多同构，但确实存在
// 「两域模型名有细微差别」与「只在某一域存在的隐藏模型」两类情况。前者靠裸名跨域
// 调度不够（同名不同 id），后者裸名在另一域必然 11102。别名表让运维在面板上手工
// 指定「对外名 → {cn 名, global 名}」，把这两类情况纳入同一套调度：
//
//	对外名 "glm-5.2"  → {cn: "glm-5.2", global: "glm-5.2-intl"}   两域名不同
//	对外名 "my-hidden" → {cn: "",       global: "internal-x"}      只在 global 存在
//
// 单域条目（某一域名留空）同时收紧候选域：该域账号根本不参与选号，避免被选中后
// 吃一次 11102 再换号（见 pool.RealmSet）。
//
// 存储与热生效：独立文件（默认 data/model_aliases.json，与 state.json 同目录），
// 5s 轮询热加载（同 auths 目录热加载模式，见 cmd/server/reload.go）。**不放 config.json**
// ——网关只在启动时读 config.json，改它必须重启；别名是高频调整的运维参数，必须热生效。
// 不放进 state.json——那是 pool 运行时状态，语义与生命周期都不同。
package aliases

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync/atomic"
)

// Entry 单条别名映射：对外名 + 两域各自的真实模型名。
// CN / Global 可各留空（该域不存在此模型），但不可同时为空（Validate 拒绝）。
type Entry struct {
	Name   string `json:"name"`   // 对外暴露的模型名（不含 ":"，见 Validate）
	CN     string `json:"cn"`     // CN 域出站名（空 = CN 域不存在）
	Global string `json:"global"` // global 域出站名（空 = global 域不存在）
}

// File 磁盘格式（整份文件即一个对象，便于面板整体读写 + 备份）。
type File struct {
	Aliases []Entry `json:"aliases"`
}

// Table 不可变别名表快照。所有读取方法无锁（由 Store 的 atomic 发布保证可见性），
// 故 Table 一旦构造完成**绝不可再修改**（Store.Set 只发布新表，不改旧表）。
type Table struct {
	entries map[string]Entry
}

// Empty 空表（无任何别名；Resolve 全部走原样透传）。
func Empty() *Table { return &Table{} }

// Lookup 按对外名查条目。
func (t *Table) Lookup(name string) (Entry, bool) {
	if t == nil || len(t.entries) == 0 {
		return Entry{}, false
	}
	e, ok := t.entries[name]
	return e, ok
}

// Resolve 把对外名解析为 realm 域的出站模型名。
//
//   - 未命中别名（或空表）→ 原样返回 (name, true)：无别名即透传，与引入前逐字一致。
//   - 命中且该域名非空   → 返回该域名。
//   - 命中但该域名为空   → ("", false)：该域不存在此模型，调用方必须跳过该域账号
//     （选号侧由 RealmSet 提前排除，本返回值是防御性兜底）。
func (t *Table) Resolve(name, realm string) (string, bool) {
	e, ok := t.Lookup(name)
	if !ok {
		return name, true
	}
	out := e.CN
	if realm == "global" {
		out = e.Global
	}
	if out == "" {
		return "", false
	}
	return out, true
}

// AllowedRealms 返回该对外名允许的域：未命中别名 → (true, true)（两域都允许，
// 即全池，与无别名语义一致）；命中 → 按条目里非空的域名。
// 供调用方构造 pool.RealmSet 收紧候选（单域条目排除另一域账号）。
func (t *Table) AllowedRealms(name string) (cn, global bool) {
	e, ok := t.Lookup(name)
	if !ok {
		return true, true
	}
	return e.CN != "", e.Global != ""
}

// Names 返回全部对外名（升序，稳定输出；供 /v1/models 与面板下拉建议）。
func (t *Table) Names() []string {
	if t == nil || len(t.entries) == 0 {
		return nil
	}
	out := make([]string, 0, len(t.entries))
	for name := range t.entries {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Entries 返回全部条目（按对外名升序；供面板读取与 /v1/models 域标注）。
func (t *Table) Entries() []Entry {
	names := t.Names()
	if len(names) == 0 {
		return nil
	}
	out := make([]Entry, 0, len(names))
	for _, n := range names {
		out = append(out, t.entries[n])
	}
	return out
}

// Len 条目数（观测/测试用）。
func (t *Table) Len() int {
	if t == nil {
		return 0
	}
	return len(t.entries)
}

// Parse 解析别名文件内容为不可变表。
//
// 校验规则（任何一条不满足即整体拒绝——**保持旧表**，绝不半量生效）：
//   - name 非空、不含 ":"（"cn:"/"global:" 是网关路由前缀，别名键与之冲突会让
//     前缀协议歧义：名为 "cn:foo" 的别名永远轮不到被查，因为请求先被前缀解析掉）；
//   - cn / global 至少一个非空（两者皆空 = 该对外名没有任何域能出站）；
//   - name 不重复。
//
// cn / global 的值**允许**含 ":"：它们是出站名，直接写进上游请求体，不经网关前缀
// 解析（既有裸名如 "deepseek:v3" 就带冒号，属合法出站名）。
//
// 空文件 / 空 aliases 数组是合法输入（清空别名表）。
func Parse(data []byte) (*Table, error) {
	var f File
	if len(strings.TrimSpace(string(data))) == 0 {
		return Empty(), nil // 空文件 = 无别名（不是错误：文件可能刚被创建）
	}
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("解析别名文件: %w", err)
	}
	t := &Table{entries: make(map[string]Entry, len(f.Aliases))}
	for i, e := range f.Aliases {
		name := strings.TrimSpace(e.Name)
		cn := strings.TrimSpace(e.CN)
		gl := strings.TrimSpace(e.Global)
		if name == "" {
			return nil, fmt.Errorf("aliases[%d]: name 为空", i)
		}
		if strings.Contains(name, ":") {
			return nil, fmt.Errorf("aliases[%d]: name %q 含 %q（与 cn:/global: 路由前缀冲突）", i, name, ":")
		}
		if cn == "" && gl == "" {
			return nil, fmt.Errorf("aliases[%d]: %q 的 cn/global 不可同时为空", i, name)
		}
		if _, dup := t.entries[name]; dup {
			return nil, fmt.Errorf("aliases[%d]: name %q 重复", i, name)
		}
		t.entries[name] = Entry{Name: name, CN: cn, Global: gl}
	}
	return t, nil
}

// Load 读取并解析别名文件。
//
// 文件不存在视为空表（**不是错误**）：默认部署没有别名文件，不该因此起不来或刷错误日志。
// 解析失败（非法 JSON / 校验不过）返回错误——调用方据此**保持旧表**并打 WARN，
// 绝不因一次误编辑把别名清空（面板写坏文件时线上仍按旧映射跑）。
func Load(path string) (*Table, error) {
	if path == "" {
		return Empty(), nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Empty(), nil
		}
		return nil, err
	}
	return Parse(data)
}

// Signature 文件指纹（size + mtime），供热加载轮询判断是否需要重载。
// 文件不存在返回 "missing"（与"存在但为空"区分：从无到有也要触发一次重载）。
func Signature(path string) string {
	if path == "" {
		return ""
	}
	fi, err := os.Stat(path)
	if err != nil {
		return "missing"
	}
	return fmt.Sprintf("%d|%d", fi.Size(), fi.ModTime().UnixNano())
}

// Store 并发安全的别名表持有者：atomic.Value 发布不可变 *Table 快照。
// 读路径（每个 chat 请求）无锁；写路径只在热加载/启动时发生。
type Store struct {
	v atomic.Value // 存 *Table，永不为 nil（NewStore 即发布空表）
}

// NewStore 构建空表 Store（未加载任何别名；Resolve 全部透传）。
func NewStore() *Store {
	s := &Store{}
	s.v.Store(Empty())
	return s
}

// Set 发布新表（nil 视为清空）。并发读方看到的是新表或旧表，绝不看到中间态。
func (s *Store) Set(t *Table) {
	if t == nil {
		t = Empty()
	}
	s.v.Store(t)
}

// Get 取当前表（永不为 nil）。
func (s *Store) Get() *Table {
	t, _ := s.v.Load().(*Table)
	if t == nil {
		return Empty()
	}
	return t
}

// Resolve 当前表的 Resolve 转发（调用方无需自己 Get）。
func (s *Store) Resolve(name, realm string) (string, bool) {
	return s.Get().Resolve(name, realm)
}

// AllowedRealms 当前表的 AllowedRealms 转发。
func (s *Store) AllowedRealms(name string) (cn, global bool) {
	return s.Get().AllowedRealms(name)
}
