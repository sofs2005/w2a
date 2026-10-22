package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/scheduler"
	"workbuddy2api/internal/server"
)

// newReloadFixture 搭一套「已装配好」的热重载目标（池 / handler / 调度器 / holder），
// 以及一份写在临时目录里的 config.json。
func newReloadFixture(t *testing.T, body string) (string, configTargets) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("初始配置非法: %v", err)
	}
	p := pool.New("")
	up := buildUpstream(loaded)
	h := server.NewHandler(server.Config{Pool: p, Upstream: up, APIKey: loaded.APIKey})
	sch := scheduler.New(schedulerConfig(loaded))
	return path, configTargets{
		pool:    p,
		up:      newUpstreamHolder(up),
		handler: h,
		sched:   sch,
		loaded:  loaded,
	}
}

// writeConfig 覆盖写配置文件（面板用的是原子替换，这里直接覆盖等价）。
func writeConfig(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestReloadConfigAppliesHotFields 热生效的核心契约：面板保存后
// api_key / 冷却基数 / 排程段立即换掉，且基线随之前进（重存同样内容不再重复应用）。
func TestReloadConfigAppliesHotFields(t *testing.T) {
	path, tgt := newReloadFixture(t, `{"api_key":"old","cooldown":{"soft_rate":"600s"},
		"schedule":{"checkin_hours":[9,21],"travel_hours":[9],"activity_hours":[10],
		"keepalive_hours":[22],"cat_hours":[1],"growth_hours":[8],"limited_hours":[12]}}`)
	a := newConfigReloaderAdapter(path, tgt)

	writeConfig(t, path, `{"api_key":"new","cooldown":{"soft_rate":"90s"},
		"schedule":{"checkin_hours":[7],"travel_hours":[9],"activity_hours":[10],
		"keepalive_hours":[22],"cat_hours":[1],"growth_hours":[8],"limited_hours":[12]}}`)
	if err := a.ReloadConfig(); err != nil {
		t.Fatalf("ReloadConfig: %v", err)
	}

	if got := tgt.handler.Config().APIKey; got != "new" {
		t.Errorf("api_key 未热生效: %q want new", got)
	}
	if got := tgt.handler.Config().SoftCooldown; got != 90*time.Second {
		t.Errorf("soft_rate 未热生效: %v want 90s", got)
	}
	// 排程段进了调度器（经 Reload 替换快照）。注意不能用「下一个时点的小时数」断言：
	// 随机平移（random_delay_minutes，默认非 0）会把时点挪到 7:0x，跨天时还可能变成
	// 21:xx——那是正确行为，不是没生效。
	if got := tgt.sched.CheckinHoursForTest(); len(got) != 1 || got[0] != 7 {
		t.Errorf("排程未热生效: checkin_hours=%v want [7]", got)
	}

	// diff 基准已前进：同样的内容再保存一次不做任何事（上游客户端不会被再重建一次）。
	before := tgt.up.Load()
	if err := a.ReloadConfig(); err != nil {
		t.Fatalf("重复保存应成功: %v", err)
	}
	if tgt.up.Load() != before {
		t.Error("内容未变却重建了上游客户端（diff 基准没前进）")
	}
}

// TestReloadConfigKeepsCurrentOnBrokenJSON 写坏文件时保持现状（绝不清空配置），
// 并如实返回错误——面板据此提示用户「没生效」。
func TestReloadConfigKeepsCurrentOnBrokenJSON(t *testing.T) {
	path, tgt := newReloadFixture(t, `{"api_key":"keep-me"}`)
	a := newConfigReloaderAdapter(path, tgt)
	before := tgt.handler.Config().APIKey
	if before != "keep-me" {
		t.Fatalf("前置状态错: api_key=%q", before)
	}

	writeConfig(t, path, `{"api_key":`)
	if err := a.ReloadConfig(); err == nil {
		t.Error("坏文件必须返回错误（面板据此提示用户）")
	}
	if got := tgt.handler.Config().APIKey; got != before {
		t.Errorf("坏配置污染了生效配置: api_key=%q want %q", got, before)
	}

	// 修好后再次保存：正常生效。
	writeConfig(t, path, `{"api_key":"fixed"}`)
	if err := a.ReloadConfig(); err != nil {
		t.Fatalf("修复后 ReloadConfig: %v", err)
	}
	if got := tgt.handler.Config().APIKey; got != "fixed" {
		t.Errorf("修复后未重载: api_key=%q want fixed", got)
	}
}

// TestReloadConfigStructuralFieldNeedsRestart 只改了 listen 这类结构性字段时
// **不切换任何东西**（连可热字段也不动），避免网关卡在两种配置之间；
// 但不算失败（返回 nil），日志里会点名需要重启。
func TestReloadConfigStructuralFieldNeedsRestart(t *testing.T) {
	path, tgt := newReloadFixture(t, `{"listen":":7863","api_key":"a"}`)
	a := newConfigReloaderAdapter(path, tgt)

	writeConfig(t, path, `{"listen":":9000","api_key":"a"}`)
	if err := a.ReloadConfig(); err != nil {
		t.Fatalf("只改结构性字段不算失败: %v", err)
	}
	if got := tgt.loaded.Listen; got != ":7863" {
		t.Errorf("结构性改动被应用了: listen=%q want :7863", got)
	}
}

// TestConfigDiffReportsTopLevelFields diff 要能指出变了的顶层字段（含结构性字段），
// 这是「是否需要重启」的唯一判据。
func TestConfigDiffReportsTopLevelFields(t *testing.T) {
	a, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	b, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if got := configDiff(a, b); len(got) != 0 {
		t.Fatalf("两份相同默认配置不应有 diff: %v", got)
	}

	b.APIKey = "k"
	b.Pool.MaxInFlight = 7
	b.SessionSticky.TTL = "10m"
	got := configDiff(a, b)
	want := map[string]bool{"api_key": true, "pool": true, "session_sticky": true}
	if len(got) != len(want) {
		t.Fatalf("diff=%v want keys %v", got, want)
	}
	for _, f := range got {
		if !want[f] {
			t.Errorf("意外字段 %q in diff %v", f, got)
		}
	}
}

// TestSplitHotAndCold 结构性字段必须被分到 cold 组（其余一律 hot）。
func TestSplitHotAndCold(t *testing.T) {
	hot, cold := splitHotAndCold([]string{
		"api_key", "listen", "upstream", "auth_dir", "state_file", "upstash", "global",
	})
	if len(cold) != 4 {
		t.Errorf("cold=%v want 4 项（listen/auth_dir/state_file/upstash）", cold)
	}
	if len(hot) != 3 {
		t.Errorf("hot=%v want 3 项", hot)
	}
}
