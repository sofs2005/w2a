// routes_config.go 一次性运维工具（signin / credit / trial / activity / login）的
// 线路表数据源。
//
// 网关自己走 cmd/server/config.go（带启动即报错的完整校验）；这些工具不加载网关
// 配置，但**同样会按账号的 ProxyName 选 Transport**（doJSON / clientFor 是共享的）。
// 若不注入线路表，「面板给号绑了线路」的部署上这些工具会逐个报
// route_unavailable —— 而它们本可以正常经该线路出网。
//
// 数据源与网关同口径、同优先级：`WB2A_PROXIES`（JSON 对象）覆盖 config.json 的
// `proxies` 段（与 applyEnv 的应用顺序一致）。
package upstream

import (
	"encoding/json"
	"log"
	"os"
	"strings"
)

// LoadRouteTable 从 config.json（path 为空时取 WB2A_CONFIG，再回落 ./config.json）
// 的 proxies 段加载线路表，随后用 WB2A_PROXIES 覆盖。文件不可读/无该段都静默跳过
// （这些工具本来就不强制要求 config.json）。
//
// 非法条目这里**跳过并 WARN**，不 fail-fast：网关侧已经启动即报错，工具侧再挡一次
// 只会让运维连签名/查积分都做不了。被跳过的线路名在用时仍会明确报错（TransportFor）。
func LoadRouteTable(path string) {
	if path == "" {
		path = os.Getenv("WB2A_CONFIG")
	}
	if path == "" {
		path = "config.json"
	}
	table := map[string]string{}
	if raw, err := os.ReadFile(path); err == nil {
		var doc struct {
			Proxies map[string]string `json:"proxies"`
		}
		if err := json.Unmarshal(raw, &doc); err == nil {
			for k, v := range doc.Proxies {
				table[k] = v
			}
		}
	}
	// env 覆盖文件（与 cmd/server 的 applyEnv 同序）
	if v := strings.TrimSpace(os.Getenv("WB2A_PROXIES")); v != "" {
		var envTable map[string]string
		if err := json.Unmarshal([]byte(v), &envTable); err == nil {
			table = envTable
		} else {
			log.Printf("WARN: [upstream] WB2A_PROXIES 不是合法 JSON 对象，忽略：%v", err)
		}
	}
	clean := make(map[string]string, len(table))
	for name, addr := range table {
		n, v := strings.TrimSpace(name), strings.TrimSpace(addr)
		if n == "" || v == "" {
			continue
		}
		if _, err := normalizeProxyURL(v); err != nil {
			log.Printf("WARN: [upstream] 出口线路 %q 地址非法，已跳过（该线路上的账号会明确报错）：%v", n, err)
			continue
		}
		clean[n] = v
	}
	ConfigureProxies(clean)
}
