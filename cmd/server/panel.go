// panel.go 把 workbuddy2api-gui 面板挂到网关的同一监听上（单端口对外）。
//
// 设计取态：面板是 git subtree 引入的**独立 module**（见 panel/）。Go 的 internal
// 规则不允许本 module 直接 import `workbuddy2api-gui/internal/*`，故装配逻辑放在
// panel/host（非 internal 包，可被 import），本文件只做路由合并与降级处理。
//
// 这样上游文件只动 main.go 的几行，其余全在这里，以免上游每天数十次提交时冲突。
package main

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"path/filepath"
	"strings"

	panelhost "workbuddy2api-gui/host"
)

// panelVersion 面板版本，构建期可经 -ldflags "-X main.panelVersion=..." 注入。
var panelVersion = "dev"

// loopbackURL 由网关监听地址推导回环 URL。
//
// 面板作为网关的客户端，需要一个能访问到本进程的地址。监听地址可能是 ":7863"、
// "0.0.0.0:7863" 或 "127.0.0.1:7863"，一律归一化为 127.0.0.1 上的同端口：
// 面板与网关同进程，走回环最短且不受防火墙影响。
func loopbackURL(listen string) (string, error) {
	_, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "", fmt.Errorf("解析监听地址 %q: %w", listen, err)
	}
	if port == "" {
		return "", fmt.Errorf("监听地址 %q 缺少端口", listen)
	}
	return "http://127.0.0.1:" + port, nil
}

// panelDataDir 面板自有数据的落盘目录：跟随网关 state_file 所在目录
// （Docker 里是持久化的 ./data 卷），使备份与登录凭据随容器重建保留。
func panelDataDir(cfg *Config) string {
	if cfg.StateFile != "" {
		return filepath.Dir(cfg.StateFile)
	}
	return "."
}

// trimSlash 让带尾斜杠的请求等价于不带尾斜杠的那条路由。
//
// 为什么需要：面板的 SPA 回落把未知路径一律返回 200 + index.html，所以尾斜杠形态
// 若不显式接管，`/healthz/` 会在网关完全挂掉时仍回 200 —— 任何把路径规范化出尾斜杠
// 的探针 / 监控 / 反向代理都会得到**假成功**。这与 handler.go 里 ServiceName 存在的
// 理由（识别"假成功"）直接冲突。
//
// 这里剥掉尾斜杠后转给网关 handler，使 `/healthz/` 与 `/healthz` 行为一致
// （而不是 404 —— 那会让带尾斜杠的探针一直失败）。
func trimSlash(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(r.URL.Path) > 1 && strings.HasSuffix(r.URL.Path, "/") {
			r2 := r.Clone(r.Context())
			r2.URL.Path = strings.TrimSuffix(r.URL.Path, "/")
			next.ServeHTTP(w, r2)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// newRootHandler 把网关 handler 与面板 handler 合到同一个 mux，单端口对外。
//
// 路由分配（Go 1.22 ServeMux 按「最具体优先」匹配，故网关的精确模式必然胜过
// 面板的 "/" 兜底，上游 handler.go 一行都不用改）：
//
//	/v1/       → 网关（OpenAI 兼容）
//	/admin/    → 网关（运维端点：账号停用/恢复/复活）
//	/status    → 网关（账号池状态）
//	/healthz   → 网关（探活）
//	/          → 面板（/api/* REST + /assets/* 静态 + SPA 回落）
//
// 尾斜杠形态（/status/、/healthz/）经 trimSlash 交给网关，理由见该函数注释。
func newRootHandler(cfg *Config, cfgPath string, gwHandler http.Handler) (http.Handler, error) {
	base, err := loopbackURL(cfg.Listen)
	if err != nil {
		return nil, err
	}

	panelHandler, err := panelhost.New(panelhost.Options{
		GatewayURL:    base,
		GatewayAPIKey: cfg.APIKey,
		AuthDir:       cfg.AuthDir,
		ConfigFile:    cfgPath,
		DataDir:       panelDataDir(cfg),
		Version:       panelVersion,
	})
	if err != nil {
		return nil, err
	}

	root := http.NewServeMux()
	root.Handle("/v1/", gwHandler)
	// /admin/ 必须显式路由给网关。漏掉它时请求会落到下面的面板 "/" 兜底，而面板的
	// SPA 回落把未知路径一律返回 200 + index.html —— 于是运维端点表现为「返回一坨
	// HTML 且状态码 200」，cmd/acct 与面板的停用/恢复按钮全部静默失效（曾实测：
	// POST /admin/accounts/<uid>/disable 得到的是控制台首页 HTML）。
	// 网关侧按 cfg.AdminEnabled 条件注册，未开启时自身回 404——这里的转发是无条件的，
	// 门禁留在网关，避免两处开关判断漂移。面板不使用 /admin 命名空间（已核对），
	// 故不存在抢路由。
	root.Handle("/admin/", gwHandler)
	root.Handle("/status", gwHandler)
	root.Handle("/status/", trimSlash(gwHandler))
	root.Handle("/healthz", gwHandler)
	root.Handle("/healthz/", trimSlash(gwHandler))
	root.Handle("/", panelHandler)

	if listenExternal(cfg.Listen) {
		log.Printf("  ⚠️  面板与 API 同端口且监听非回环地址：面板持有全部账号凭据，")
		log.Printf("      请务必改强口令（WBGUI_PASSWORD）并置于 HTTPS 反向代理之后")
	}
	return root, nil
}

// listenExternal 报告监听地址是否对外（非回环），用于安全提示。
func listenExternal(listen string) bool {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return true
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return !ip.IsLoopback()
	}
	return host != "localhost"
}
