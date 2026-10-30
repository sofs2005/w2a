// connect_retry_config_test.go 钉住 upstream.connect_retry / connect_retry_backoff
// 的默认值、归一与接线（E 段）。
//
// 三个口径必须一致：config 常量 ↔ config.example.json ↔ internal/server 侧默认。
// 漂移的表现是「文档说默认 2 次，实际不重试」这种最难查的形态，故这里逐条钉死。
package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"workbuddy2api/internal/server"
)

// TestConnectRetryDefaults Default() 必须显式预置「开启」语义（零值是 0 = 关闭，
// 不能靠零值兜底）。
func TestConnectRetryDefaults(t *testing.T) {
	c := Default()
	if c.Upstream.ConnectRetry != server.DefaultConnectRetry {
		t.Errorf("connect_retry 默认值 = %d want %d（server.DefaultConnectRetry）",
			c.Upstream.ConnectRetry, server.DefaultConnectRetry)
	}
	if c.Upstream.ConnectRetryBackoff != defaultUpstreamConnectRetryBackoff {
		t.Errorf("connect_retry_backoff 默认值 = %q want %q",
			c.Upstream.ConnectRetryBackoff, defaultUpstreamConnectRetryBackoff)
	}
	// 三处口径：config 常量与 server 侧默认逐值相等（这条就是防漂移闸门）。
	if defaultUpstreamConnectRetry != server.DefaultConnectRetry {
		t.Errorf("config 常量 %d != server 默认 %d", defaultUpstreamConnectRetry, server.DefaultConnectRetry)
	}
	d, err := time.ParseDuration(defaultUpstreamConnectRetryBackoff)
	if err != nil {
		t.Fatalf("退避常量不是合法时长: %v", err)
	}
	if d != server.DefaultConnectRetryBackoff {
		t.Errorf("config 退避常量 %v != server 默认 %v", d, server.DefaultConnectRetryBackoff)
	}
}

// TestConnectRetryNormalize 归一语义：0 = 关闭（合法，不回落默认）；负数钳 0；
// 退避空/非法/非正回落默认（不 fail-fast）。
func TestConnectRetryNormalize(t *testing.T) {
	c := Default()
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if c.Upstream.ConnectRetry != defaultUpstreamConnectRetry {
		t.Errorf("默认配置归一后 connect_retry = %d want %d", c.Upstream.ConnectRetry, defaultUpstreamConnectRetry)
	}
	if c.ConnectRetryBackoffDur != server.DefaultConnectRetryBackoff {
		t.Errorf("默认配置归一后 backoff = %v want %v", c.ConnectRetryBackoffDur, server.DefaultConnectRetryBackoff)
	}

	// 0 = 显式关闭：必须保留 0（不得被 <=0 回落默认吃掉）。
	dir := t.TempDir()
	write := func(name, body string) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	c0, err := Load(write("off.json", `{"upstream":{"connect_retry":0}}`))
	if err != nil {
		t.Fatalf("connect_retry=0 应合法: %v", err)
	}
	if c0.Upstream.ConnectRetry != 0 {
		t.Errorf("connect_retry=0（关闭）不得被回落成默认: got %d", c0.Upstream.ConnectRetry)
	}

	// 负数按 0（不 fail-fast）。
	cneg, err := Load(write("neg.json", `{"upstream":{"connect_retry":-3}}`))
	if err != nil {
		t.Fatalf("负值不应 fail-fast: %v", err)
	}
	if cneg.Upstream.ConnectRetry != 0 {
		t.Errorf("负数应钳 0, got %d", cneg.Upstream.ConnectRetry)
	}

	// 退避非法 / 非正：回落默认（+ WARN，不 fail-fast）。
	for name, body := range map[string]string{
		"非法": `{"upstream":{"connect_retry_backoff":"soon"}}`,
		"零值": `{"upstream":{"connect_retry_backoff":"0s"}}`,
		"负值": `{"upstream":{"connect_retry_backoff":"-1s"}}`,
		"空串": `{"upstream":{"connect_retry_backoff":""}}`,
	} {
		c, err := Load(write(name+".json", body))
		if err != nil {
			t.Fatalf("%s 不应 fail-fast: %v", name, err)
		}
		if c.ConnectRetryBackoffDur != server.DefaultConnectRetryBackoff {
			t.Errorf("%s: backoff 应回落默认 %v, got %v", name, server.DefaultConnectRetryBackoff, c.ConnectRetryBackoffDur)
		}
	}

	// 合法显式值原样生效。
	cok, err := Load(write("ok.json", `{"upstream":{"connect_retry":5,"connect_retry_backoff":"1s"}}`))
	if err != nil {
		t.Fatalf("合法值应能加载: %v", err)
	}
	if cok.Upstream.ConnectRetry != 5 || cok.ConnectRetryBackoffDur != time.Second {
		t.Errorf("显式值未生效: retry=%d backoff=%v", cok.Upstream.ConnectRetry, cok.ConnectRetryBackoffDur)
	}
}

// TestConnectRetryKeyAbsentUsesDefault 锁死「键缺席取默认、显式 0 才关闭」的口径：
// `{"upstream":{}}`（段在、键不在）→ 生效值必须是 2 / 200ms；`{"connect_retry":0}`
// → 生效值 0（关闭）。两者只差一个键，语义相反，必须各有一条直接断言钉住。
func TestConnectRetryKeyAbsentUsesDefault(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	// 键缺席：upstream 段存在但没有 connect_retry / connect_retry_backoff。
	absent, err := Load(write("absent.json", `{"upstream":{}}`))
	if err != nil {
		t.Fatalf("upstream 段为空应合法: %v", err)
	}
	if absent.Upstream.ConnectRetry != 2 {
		t.Errorf("键缺席时 connect_retry 必须取默认 2, got %d", absent.Upstream.ConnectRetry)
	}
	if absent.ConnectRetryBackoffDur != 200*time.Millisecond {
		t.Errorf("键缺席时 connect_retry_backoff 必须取默认 200ms, got %v", absent.ConnectRetryBackoffDur)
	}

	// 显式 0：关闭（不得被「<=0 回落默认」吃掉）；退避与次数解耦，仍取默认。
	off, err := Load(write("off.json", `{"upstream":{"connect_retry":0}}`))
	if err != nil {
		t.Fatalf("connect_retry=0 应合法: %v", err)
	}
	if off.Upstream.ConnectRetry != 0 {
		t.Errorf("显式 connect_retry=0 必须生效（关闭）, got %d", off.Upstream.ConnectRetry)
	}
	if off.ConnectRetryBackoffDur != 200*time.Millisecond {
		t.Errorf("显式关闭时 backoff 仍取默认 200ms, got %v", off.ConnectRetryBackoffDur)
	}
}

// TestConnectRetryEnv 环境变量覆盖（非法值忽略并保留配置文件的值）。
func TestConnectRetryEnv(t *testing.T) {
	t.Setenv("WB2A_CONNECT_RETRY", "4")
	t.Setenv("WB2A_CONNECT_RETRY_BACKOFF", "750ms")
	c, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Upstream.ConnectRetry != 4 {
		t.Errorf("WB2A_CONNECT_RETRY 未生效: %d", c.Upstream.ConnectRetry)
	}
	if c.ConnectRetryBackoffDur != 750*time.Millisecond {
		t.Errorf("WB2A_CONNECT_RETRY_BACKOFF 未生效: %v", c.ConnectRetryBackoffDur)
	}

	// 非法：忽略（不改变默认），不 fail-fast。
	t.Setenv("WB2A_CONNECT_RETRY", "many")
	t.Setenv("WB2A_CONNECT_RETRY_BACKOFF", "later")
	c2, err := Load("")
	if err != nil {
		t.Fatalf("非法 env 不应 fail-fast: %v", err)
	}
	if c2.Upstream.ConnectRetry != server.DefaultConnectRetry {
		t.Errorf("非法次数应保留默认, got %d", c2.Upstream.ConnectRetry)
	}
	if c2.ConnectRetryBackoffDur != server.DefaultConnectRetryBackoff {
		t.Errorf("非法退避应保留默认, got %v", c2.ConnectRetryBackoffDur)
	}
}

// TestConnectRetryReloadWiring 热重载接线：面板改 connect_retry 后，handler 快照
// 必须立刻拿到新值（漏接线的表现是「改了配置却不重试」，这条测试就是那道闸门）。
func TestConnectRetryReloadWiring(t *testing.T) {
	path, tgt := newReloadFixture(t, `{"upstream":{"connect_retry":1,"connect_retry_backoff":"100ms"}}`)
	a := newConfigReloaderAdapter(path, tgt)

	writeConfig(t, path, `{"upstream":{"connect_retry":3,"connect_retry_backoff":"50ms"}}`)
	hot, cold, err := a.ReloadConfig()
	if err != nil {
		t.Fatalf("ReloadConfig: %v", err)
	}
	if len(cold) != 0 {
		t.Errorf("upstream 段应可热生效, cold=%v", cold)
	}
	if len(hot) != 1 || hot[0] != "upstream" {
		t.Errorf("hot=%v want [upstream]", hot)
	}
	hc := tgt.handler.Config()
	if hc.ConnectRetry != 3 {
		t.Errorf("connect_retry 未热生效: %d want 3", hc.ConnectRetry)
	}
	if hc.ConnectRetryBackoff != 50*time.Millisecond {
		t.Errorf("connect_retry_backoff 未热生效: %v want 50ms", hc.ConnectRetryBackoff)
	}
}

// TestExampleConfigConnectRetry 示例配置的取值必须与默认值一致（运维首装抄的就是
// 它；三处口径的第三处）。
func TestExampleConfigConnectRetry(t *testing.T) {
	if _, err := os.Stat(exampleConfigPath); err != nil {
		t.Skipf("示例配置不在（%v），跳过", err)
	}
	c, err := Load(exampleConfigPath)
	if err != nil {
		t.Fatalf("config.example.json 不能通过 Load: %v", err)
	}
	if c.Upstream.ConnectRetry != server.DefaultConnectRetry {
		t.Errorf("示例配置 connect_retry=%d want %d", c.Upstream.ConnectRetry, server.DefaultConnectRetry)
	}
	if c.ConnectRetryBackoffDur != server.DefaultConnectRetryBackoff {
		t.Errorf("示例配置 connect_retry_backoff=%v want %v",
			c.ConnectRetryBackoffDur, server.DefaultConnectRetryBackoff)
	}
}
