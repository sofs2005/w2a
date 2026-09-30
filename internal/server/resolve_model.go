package server

import (
	"fmt"
	"strings"
	"sync/atomic"

	"workbuddy2api/internal/aliases"
	"workbuddy2api/internal/pool"
)

// RealmUnified 裸模型名（无前缀）的路由域标识：**全池统一调度**。
//
// 空串与 pool 层的 realm 谓词天然同义——pool.pick 的
// `realmOK := func(e) bool { return realm == "" || e.a.Realm() == realm }`
// 以及 realm.go 的全部 `realm==""` 退化分支都把空串解释为「不限域」，
// 故裸名传 "" 即可让 CN 与 global 账号参与同一套粘性/加权/分层/紧急调度，
// pool 层零改动。取空串而非新造枚举值，是为了让这条等价关系在类型上就成立
// （任何把 realm 透传给 pool 的调用方都不必翻译）。
const RealmUnified = ""

// unifiedRouting 统一调度开关（config pool.unified_routing，缺省 true）。
//
// 这是本改造的**兼容性逃生门**：裸模型名此前恒路由 CN（老客户端零回归契约），
// 改造后裸名默认进全池（CN+global 一起调度）。显式 SetUnifiedRouting(false)
// 恢复旧语义（裸名 → cn），与 auth.globalEnabled 正交：
//   - global.enabled=false 是第一道闸（账号层面锁死纯 CN，Realm() 恒 cn）；
//   - 本开关是路由层闸（裸名是否允许跨域调度），关闭后裸名只打 CN 账号。
//
// 用包级 atomic 而非逐层透传 Config：resolveModel 的调用方横跨三处
// （handler.chatCompletions、metrics.enrichCredits、cmd/server/wiring.go 的粘性闭包），
// 其中 wiring 闭包由 session.Router 回调、拿不到 handler.Config。包级闸与
// auth.SetGlobalEnabled 同模式（启动时从 config 注入一次，全进程一致）。
var unifiedRouting atomic.Bool

func init() { unifiedRouting.Store(true) }

// SetUnifiedRouting 注入统一调度开关（false = 裸名退回只路由 CN 的旧语义）。
func SetUnifiedRouting(enabled bool) { unifiedRouting.Store(enabled) }

// bareRealm 裸名（无前缀）应路由的 realm：统一调度开 → RealmUnified（全池）；
// 关 → "cn"（旧语义，老客户端零回归）。
func bareRealm() string {
	if unifiedRouting.Load() {
		return RealmUnified
	}
	return "cn"
}

// resolveModel 解析模型名协议（PLAN D6 / 统一调度改造）：
//
//	显式前缀： "[realm:]model"   realm ∈ {cn, global} → 钉域
//	裸名：     其余一律 → RealmUnified（全池统一调度；开关关闭时回落 "cn"）
//
// 取第一个 ":"，前段恰为 "cn"/"global" 才剥离（大小写敏感，精确小写枚举）；
// 否则视为裸名，realm 由 bareRealm() 决定、bare=原串——裸名不再无条件默认打 CN，
// 这是本改造的核心语义变化：同一个裸名可以在 CN 与 global 账号间自由调度。
//
// bare 即出站/选号/账本使用的模型名（别名映射在更上层 ResolveRoute 折叠）。
//
// 导出为 ResolveModel（cmd/server 粘性闭包需要），包内简写 resolveModel。
func resolveModel(model string) (realm, bare string) {
	idx := strings.IndexByte(model, ':')
	if idx < 0 {
		return bareRealm(), model
	}
	prefix := model[:idx]
	if prefix != "cn" && prefix != "global" {
		return bareRealm(), model
	}
	return prefix, model[idx+1:]
}

// ResolveModel 是 resolveModel 的导出面（跨包调用）。
func ResolveModel(model string) (realm, bare string) { return resolveModel(model) }

// Route 一次请求的完整路由裁决（前缀解析 + 别名查表 + 候选域收紧），是 chatCompletions
// 与 cmd/server 粘性闭包共用的**唯一**模型名解析入口（两处必须同口径，否则粘性会把
// 会话钉到选号永远不会选的域）。
//
// 字段语义：
//   - Realm：钉域（"cn"/"global"）；RealmUnified（空串）= 全池（裸名，统一调度开）。
//   - Name：**对外名**——选号、粘性、成本账本、6004/11102 键统一用它（别名请求按对外名
//     入账，同一账号在两域跑同一对外名的成本观测合并，符合"统一调度"语义）。
//   - Realms：候选域集合。裸名/前缀名未命中别名 → nil（全池，或前缀名钉域）；命中别名 →
//     按条目非空域名收紧（单域别名排除另一域账号，避免选中后吃 11102 再换号）。
//   - Aliased：是否命中别名表（出站名改写 / 目录域标注用）。
//   - Alias：命中的条目（零值 + Aliased=false 表示未命中）。
//   - Err：单域别名被显式钉到不存在的那一域（如 "cn:my-hidden" 而该别名只有 global 名）
//     —— 无任何账号能承接，调用方直接 400，不浪费一轮选号。
//
// 未命中别名时 Realms 为 nil 而 Realm 是单域：候选集由 pick 的 realms 参数表达
// （nil=全池，单域=钉域），两者不冲突——Realm 只描述"请求钉在哪个域"，
// Realms 描述"别名额外收紧了哪些域"。
func ResolveRoute(store *aliases.Store, model string) (Route, error) {
	realm, name := resolveModel(model)
	r := Route{Realm: realm, Name: name}
	if store == nil {
		return r, nil
	}
	e, ok := store.Get().Lookup(name)
	if !ok {
		return r, nil
	}
	r.Aliased = true
	r.Alias = e
	cnOK, glOK := e.CN != "", e.Global != ""
	switch {
	case cnOK && glOK:
		// 双域条目：不额外收紧（Realm 本身决定范围）。
	case realm == "global" && !glOK:
		return r, fmt.Errorf("模型 %q 在 global 域不可用（别名 %q 未配置 global 名）", name, name)
	case realm == "cn" && !cnOK:
		return r, fmt.Errorf("模型 %q 在 CN 域不可用（别名 %q 未配置 cn 名）", name, name)
	default:
		// 裸名（全池）或钉域与该域名匹配 → 收紧到该单域。
		if cnOK {
			r.Realms = pool.RealmSet{"cn": true}
		} else {
			r.Realms = pool.RealmSet{"global": true}
		}
	}
	return r, nil
}

// Route 是 ResolveRoute 的返回结构（见其注释）。零值 + Err=nil 表示"无别名、裸名全池"。
type Route struct {
	Realm   string        // 钉域：cn / global / RealmUnified（全池）
	Name    string        // 对外名（选号/粘性/账本键）
	Realms  pool.RealmSet // 别名收紧后的候选域集合；nil = 不额外收紧
	Aliased bool          // 是否命中别名表
	Alias   aliases.Entry // 命中的条目（Aliased=false 时零值）
}

// CandidateRealms 把 Realm 与别名收紧合并成 pool 选号用的域集合：
// 别名收紧优先（单域条目必须排除另一域账号）；无别名收紧时退化为单域（Realm 非空）
// 或全池（RealmUnified → nil）。
func (r Route) CandidateRealms() pool.RealmSet {
	if len(r.Realms) > 0 {
		return r.Realms
	}
	return pool.SingleRealm(r.Realm)
}

// AllowsRealm 报告某域的账号是否可承接本请求（粘性命中校验用）。
//
// 必须与 CandidateRealms 同口径，否则会出现"选号不会选的域被粘性钉住"：
// 裸名（全池）此前用 `realm != ""` 判断跳过校验，但别名收紧后裸名也可能只允许单域，
// 故统一走本方法——空集合（全池）恒允许，非空集合按域名判定。
func (r Route) AllowsRealm(realm string) bool {
	return r.CandidateRealms().Allows(realm)
}

// Outbound 给出该请求在 accountRealm 域账号上的出站模型名。
//
// 用 Route 自带的条目快照而非回查 store：一次请求内别名表可能被热加载换掉，
// 回查会拿到与 Realms（已按旧表收紧）不一致的新映射，出现"候选集按旧表、出站名
// 按新表"的错配。快照保证同一次请求内路由裁决自洽。
//
//   - 未命中别名 → 对外名原样（裸名/前缀名剥前缀后的语义，与引入前逐字一致）。
//   - 命中别名 → 取该域名；该域名为空 → ("", false)，调用方必须跳过该账号
//     （正常情况下候选集已由 CandidateRealms 排除，此处是防御性兜底）。
func (r Route) Outbound(accountRealm string) (string, bool) {
	if !r.Aliased {
		return r.Name, true
	}
	out := r.Alias.CN
	if accountRealm == "global" {
		out = r.Alias.Global
	}
	if out == "" {
		return "", false
	}
	return out, true
}
