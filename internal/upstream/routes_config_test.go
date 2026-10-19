package upstream

import (
	"os"
	"path/filepath"
	"testing"
)

// LoadRouteTable：一次性运维工具（signin / credit / trial / activity / login）的
// 线路表数据源。它们不经过 cmd/server/config.go，但同样按账号的 ProxyName 选
// Transport —— 不注入就会在「面板绑了线路」的部署上逐个报 route_unavailable。
func TestLoadRouteTableFromFile(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "config.json")
	raw := `{"proxies":{"route-a":"socks5h://u:p@a.example:1080"," route-b ":" http://b.example:8080 "}}`
	if err := os.WriteFile(fp, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ConfigureProxies(nil) })

	LoadRouteTable(fp)
	names := RouteNames()
	if len(names) != 2 || names[0] != "route-a" || names[1] != "route-b" {
		t.Fatalf("线路表没加载对（名字应去空白并排序）：%v", names)
	}
	tr, err := TransportFor("route-b")
	if err != nil {
		t.Fatalf("route-b: %v", err)
	}
	if tr.Proxy == nil {
		t.Error("route-b 应配了代理")
	}
}

// 非法地址跳过 + WARN，不 fail-fast：工具侧再挡一次只会让运维连签到都做不了；
// 被跳过的线路名在用时仍会明确报错。
func TestLoadRouteTableSkipsBadEntries(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "config.json")
	raw := `{"proxies":{"bad":"ftp://x.example:21","ok":"http://ok.example:3128"}}`
	if err := os.WriteFile(fp, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ConfigureProxies(nil) })

	LoadRouteTable(fp)
	names := RouteNames()
	if len(names) != 1 || names[0] != "ok" {
		t.Fatalf("非法条目应被跳过：%v", names)
	}
	if _, err := TransportFor("bad"); err == nil {
		t.Error("被跳过的线路在用时必须明确报错，不能静默直连")
	}
}

// 环境变量覆盖文件表（与 cmd/server 的 applyEnv 同序）。
func TestLoadRouteTableEnvOverridesFile(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "config.json")
	if err := os.WriteFile(fp, []byte(`{"proxies":{"from-file":"http://f.example:1"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WB2A_PROXIES", `{"from-env":"socks5://e.example:1080"}`)
	t.Cleanup(func() { ConfigureProxies(nil) })

	LoadRouteTable(fp)
	if names := RouteNames(); len(names) != 1 || names[0] != "from-env" {
		t.Fatalf("env 应整体覆盖文件表：%v", names)
	}
}

// 文件不存在/没有 proxies 段：静默得到空表（这些工具不强制要求 config.json）。
func TestLoadRouteTableMissingFileIsNoop(t *testing.T) {
	t.Cleanup(func() { ConfigureProxies(nil) })
	LoadRouteTable(filepath.Join(t.TempDir(), "nope.json"))
	if names := RouteNames(); len(names) != 0 {
		t.Errorf("缺文件时应为空表：%v", names)
	}
}
