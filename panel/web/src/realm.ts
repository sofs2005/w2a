// realm.ts 域（国际版 / 国内版）判定的单一出处。
//
// 两个页面（模型与倍率、积分到期）都要按域分栏，判定口径必须一致：
// 模型 id 靠前缀（global: / cn:），账号靠 domain（workbuddy.ai = 国际版）。
import type { Account } from './types'

export type Realm = 'global' | 'cn'

export const REALMS: { key: Realm; label: string; upstream: string }[] = [
  { key: 'global', label: '国际版', upstream: 'workbuddy.ai' },
  { key: 'cn', label: '国内版', upstream: 'codebuddy.cn' },
]

/** 模型 id 的域前缀判定：global: 为国际版，其余（含无前缀 / cn:）为国内版。 */
export function modelRealm(id: string): Realm {
  return id.startsWith('global:') ? 'global' : 'cn'
}

/** 去掉域前缀，得到调用上游时真正使用的裸名。 */
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
