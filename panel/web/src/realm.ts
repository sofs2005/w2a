// realm.ts 域（国际版 / 国内版）判定的单一出处。
//
// 两个页面（模型与倍率、积分到期）都要按域分栏，判定口径必须一致：
// 账号靠 domain（workbuddy.ai = 国际版）；模型靠**服务端下发的 `realms` 字段**。
import type { Account, Model } from './types'

export type Realm = 'global' | 'cn'

export const REALMS: { key: Realm; label: string; upstream: string }[] = [
  { key: 'global', label: '国际版', upstream: 'workbuddy.ai' },
  { key: 'cn', label: '国内版', upstream: 'codebuddy.cn' },
]

/**
 * 模型可用的域集合。
 *
 * 统一调度改造后 `/v1/models` 只输出裸名，同一模型两域都有时只出现一次，
 * 靠 `realms` 区分——所以**不能**再从 id 前缀推域（前缀已经不存在了）。
 *
 * 回退：`realms` 缺席（老网关）时按 id 前缀推——`global:` 为国际版、其余为国内版，
 * 与改造前的口径一致。这样面板对两个版本的网关都能正确分栏。
 */
export function modelRealms(m: Model): Realm[] {
  if (m.realms && m.realms.length > 0) {
    const out: Realm[] = []
    for (const r of REALMS) if (m.realms.includes(r.key)) out.push(r.key)
    // 服务端若下发了未知域（未来的新域），不至于让该模型在所有 tab 里消失：
    // 退化为按前缀推断。
    if (out.length > 0) return out
  }
  return [(m.id || '').startsWith('global:') ? 'global' : 'cn']
}

/** 去掉域前缀，得到调用上游时真正使用的裸名（裸名 id 原样返回）。 */
export function bareID(id: string): string {
  return id.replace(/^(global|cn):/, '')
}

/** 账号域判定：domain 含 workbuddy.ai 为国际版，其余（copilot.tencent.com 等）为国内版。 */
export function accountRealm(a: Account): Realm {
  return (a.domain || '').toLowerCase().includes('workbuddy.ai') ? 'global' : 'cn'
}

/** 域的中文名，找不到返回原值。 */
export function realmLabel(r: Realm): string {
  return REALMS.find((x) => x.key === r)?.label ?? r
}
