package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 账号级出口线路表（config 的 proxies 段）。
//
// 与域级 proxy 的「WARN + 置空回落」不同：线路是账号级强绑定，配错后静默直连等于
// 「以为隔离了其实没隔离」——这是本功能唯一要避免的结果，故启动即报错。

func TestProxiesDefaultsEmpty(t *testing.T) {
	c := Default()
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if len(c.Proxies) != 0 {
		t.Errorf("未配 proxies 时应为空表，got %v", c.Proxies)
	}
}

func TestProxiesParsedFromFile(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	raw := `{"proxies":{" route-a ":" socks5h://u:p@a.example:1080 ","route-b":"http://b.example:8080"}}`
	if err := os.WriteFile(fp, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	// 名字与地址都去空白；地址原样保留（含 socks5h，归一化在 upstream.TransportFor 做）
	if c.Proxies["route-a"] != "socks5h://u:p@a.example:1080" {
		t.Errorf("route-a=%q（应去空白且保留原 scheme）", c.Proxies["route-a"])
	}
	if c.Proxies["route-b"] != "http://b.example:8080" {
		t.Errorf("route-b=%q", c.Proxies["route-b"])
	}
	if _, ok := c.Proxies[" route-a "]; ok {
		t.Error("名字没去空白")
	}
}

// 非法地址必须让启动失败（而不是回落成直连）。
func TestProxiesInvalidFailsStartup(t *testing.T) {
	cases := []struct{ name, addr string }{
		{"bad scheme", "ftp://x.example:21"},
		{"no host", "socks5://"},
		{"unparseable", "http://[::1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			fp := filepath.Join(dir, "c.json")
			raw := `{"proxies":{"bad":"` + tc.addr + `"}}`
			if err := os.WriteFile(fp, []byte(raw), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(fp); err == nil {
				t.Fatal("非法线路地址必须让启动报错（静默回落 = 以为隔离了其实没有）")
			} else if !strings.Contains(err.Error(), "bad") {
				t.Errorf("错误里要带线路名，便于对照配置：%v", err)
			}
		})
	}
}

// 空名/空地址的条目直接剔除（不报错）：面板编辑中途可能留下空行。
func TestProxiesDropsEmptyEntries(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	if err := os.WriteFile(fp, []byte(`{"proxies":{"":"http://a.example:1","route-a":"  ","route-b":"http://b.example:2"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Proxies) != 1 || c.Proxies["route-b"] != "http://b.example:2" {
		t.Errorf("空格条目应被剔除：%v", c.Proxies)
	}
}

// WB2A_PROXIES：JSON 对象形态的环境变量覆盖文件值。
func TestProxiesEnvOverride(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	if err := os.WriteFile(fp, []byte(`{"proxies":{"route-a":"http://file.example:1"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WB2A_PROXIES", `{"route-env":"socks5://env.example:1080"}`)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Proxies["route-env"] != "socks5://env.example:1080" {
		t.Errorf("env 线路没生效：%v", c.Proxies)
	}
	if _, ok := c.Proxies["route-a"]; ok {
		t.Error("env 应整体覆盖文件表，而不是合并")
	}
}

// 保留名 __direct__ 不需要在表里配置；表里写了也不影响（TransportFor 内建处理）。
func TestProxiesDoesNotRequireDirectEntry(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	if err := os.WriteFile(fp, []byte(`{"proxies":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Proxies) != 0 {
		t.Errorf("空表应保持空：%v", c.Proxies)
	}
}
