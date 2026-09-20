// state_flush_config_test.go pool.state_flush 配置测试（落盘周期）：
// 默认 30m / 文件覆盖 / "0" 关闭后台落盘 / 空值回落默认 / 非法值报错 / env 覆盖。
//
// 为什么这个配置值得单独钉：它是「进程被强杀时的状态丢失窗口」的**唯一**旋钮。
// 默认值从 5s 放宽到 30m 是本改动的一部分，若解析/回落出错（例如显式 "" 没兜住、
// "0" 被当成非法值报错），用户要么拿不到预期的降频、要么配置直接起不来。
package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestStateFlushDefault 键缺席 → 默认 30m。
func TestStateFlushDefault(t *testing.T) {
	c := Default()
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if c.StateFlushDur != 30*time.Minute {
		t.Errorf("state_flush=%v want 30m (default)", c.StateFlushDur)
	}
}

// TestStateFlushParsedFromFile 显式配置覆盖默认。
func TestStateFlushParsedFromFile(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"pool":{"state_flush":"45s"}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.StateFlushDur != 45*time.Second {
		t.Errorf("state_flush=%v want 45s", c.StateFlushDur)
	}
}

// TestStateFlushZeroDisables "0" = 关闭后台落盘（仅靠退出时 Flush / 显式 Flush）。
// 这是合法值而非非法值：只读/调试部署希望完全掌控写盘时机。
func TestStateFlushZeroDisables(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"pool":{"state_flush":"0"}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.StateFlushDur != 0 {
		t.Errorf("state_flush=%v want 0 (关闭后台落盘)", c.StateFlushDur)
	}
}

// TestStateFlushEmptyFallsBackToDefault 显式空串回落默认 30m。
func TestStateFlushEmptyFallsBackToDefault(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"pool":{"state_flush":""}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.StateFlushDur != 30*time.Minute {
		t.Errorf("state_flush=%v want 30m fallback", c.StateFlushDur)
	}
}

// TestStateFlushNegativeClampsToZero 负值钳 0（同关停；"-5m" 无合理语义）。
func TestStateFlushNegativeClampsToZero(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"pool":{"state_flush":"-5m"}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.StateFlushDur != 0 {
		t.Errorf("state_flush=%v want 0（负值应钳 0）", c.StateFlushDur)
	}
}

// TestBadStateFlush 非法值 fail fast（不静默回落，风格同 cost_explore_interval）。
func TestBadStateFlush(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"pool":{"state_flush":"oops"}}`), 0o600)
	if _, err := Load(fp); err == nil {
		t.Fatal("want error for bad state_flush")
	}
}

// TestStateFlushEnvOverride WB2A_STATE_FLUSH 覆盖文件值。
func TestStateFlushEnvOverride(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"pool":{"state_flush":"30m"}}`), 0o600)
	t.Setenv("WB2A_STATE_FLUSH", "90s")
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.StateFlushDur != 90*time.Second {
		t.Errorf("state_flush=%v want 90s（env 应覆盖文件）", c.StateFlushDur)
	}
}
