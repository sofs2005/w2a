// preserve.go 写回凭证时保留本包未建模的键。
//
// 为什么需要：面板的 Save 是**从自己的 struct 重建整份文档**再覆盖原文件，而磁盘上
// 的凭证还带着面板没有字段的键 —— 网关的 SaveAtomic 会写 auth.realm、顶层
// device_token 与顶层 proxy（见 workbuddy2api/internal/auth/auth.go），面板三者
// 都不建模。于是用户在面板点一次「刷新」（refreshAccount → store.Save）就会把它们
// 从磁盘上抹掉：
//
//   - realm：网关能从 domain 反推（BackfillRealm），下次自身刷新时会补回，可自愈；
//   - device_token：**不可反推**，丢失即永久失去该账号的 X-Device-Token 风控头；
//   - proxy：**不可反推**（账号级出站代理，手写配置），丢失即静默回落到域级代理，
//     表现为「这个号突然不走专用出口了」，且没有任何报错——最难查的一类。
//
// 因此这里按「保留未知键」而非「补三个硬编码字段」来修：前者对上游今后新增的键
// 同样有效，不会每加一个字段就要改一次面板。
package authstore

import (
	"encoding/json"
	"os"
)

// readExistingDoc 读取并解析磁盘上已有的凭证文档；不存在 / 不可读 / 非对象时返回 nil。
//
// 返回 nil 表示「没有可保留的旧内容」（新建账号、首次落盘），此时 Save 按 struct
// 写全新文档即可。
func readExistingDoc(path string) map[string]any {
	if path == "" {
		return nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil
	}
	return doc
}

// mergeMissingKeys 把 src 中 dst 尚未覆盖的键补进 dst，对同名子对象递归一层。
//
// 只在 dst 缺该键时补：面板明确管理的字段（accessToken / expiresAt / uid …）永远
// 以自己的值为准，不会被磁盘旧值回滚。递归是为了处理嵌套形（"auth" / "account"）。
func mergeMissingKeys(dst, src map[string]any) {
	for k, v := range src {
		cur, exists := dst[k]
		if !exists {
			dst[k] = v
			continue
		}
		// 同名且双方都是对象 → 递归合并其内部未覆盖的键。
		dm, dok := cur.(map[string]any)
		sm, sok := v.(map[string]any)
		if dok && sok {
			mergeMissingKeys(dm, sm)
		}
	}
}
