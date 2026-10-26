package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"workbuddy2api-gui/internal/config"
	"workbuddy2api-gui/internal/ops"
)

// errBoom 模拟网关侧解析/应用失败。
var errBoom = errors.New("重新解析 config.json 失败: 语法错误")

// fakeReloader 假的网关热重载能力：返回预置的 hot/cold，或预置错误。
//
// 面板的提示语完全由这三个返回值决定，所以必须能在不启动真网关的前提下把
// 「只热」「只冷」「混合」「失败」「未注入」五种情形都摆出来。
type fakeReloader struct {
	hot, cold []string
	err       error
	calls     int
}

func (f *fakeReloader) ReloadConfig() ([]string, []string, error) {
	f.calls++
	return f.hot, f.cold, f.err
}

// newConfigTestServer 起一个把网关 config.json 指向临时文件的 Server。
// reloader 传 nil 模拟独立面板模式（宿主未注入回调）。
func newConfigTestServer(t *testing.T, reloader ops.ConfigReloader, dangerous bool) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	gwCfg := filepath.Join(dir, "config.json")
	if err := os.WriteFile(gwCfg, []byte(`{"api_key":"k"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.UI.Username = "admin"
	cfg.UI.Password = "pw123456"
	cfg.UI.TTL = 0
	cfg.UpstreamConfigFile = gwCfg
	cfg.DangerousOps = dangerous
	cfg.BackupDir = filepath.Join(dir, "backups")

	svc := ops.New(cfg, nil, nil, nil)
	if reloader != nil {
		svc.SetConfigReloader(reloader)
	}
	return NewServer(cfg, svc, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), "test"), gwCfg
}

// TestConfigPutHotOnly 纯热改动只回「已生效」，不得出现「重启」字样 —— 这正是
// 用户明确要求「立即生效的不用写太复杂」的那条路径。
func TestConfigPutHotOnly(t *testing.T) {
	r := &fakeReloader{hot: []string{"api_key", "pool"}}
	s, _ := newConfigTestServer(t, r, false)

	w, resp := aliasReqPath(t, s, http.MethodPut, "/api/config", `{"api_key":"new"}`)
	if w.Code != 200 {
		t.Fatalf("code=%d want 200 body=%s", w.Code, w.Body)
	}
	if resp["restart_required"] != false {
		t.Errorf("restart_required=%v want false", resp["restart_required"])
	}
	if resp["reloaded"] != true {
		t.Errorf("reloaded=%v want true", resp["reloaded"])
	}
	msg, _ := resp["message"].(string)
	if !strings.Contains(msg, "已生效") {
		t.Errorf("提示应说明已生效: %q", msg)
	}
	if strings.Contains(msg, "重启") {
		t.Errorf("纯热改动不应提重启: %q", msg)
	}
	if _, ok := resp["restart_fields"]; ok {
		t.Errorf("无需重启时不应下发 restart_fields: %v", resp["restart_fields"])
	}
}

// TestConfigPutColdOnly 只改冷字段：网关一个都没应用，提示必须说需重启并点名。
func TestConfigPutColdOnly(t *testing.T) {
	r := &fakeReloader{cold: []string{"listen", "upstash"}}
	s, _ := newConfigTestServer(t, r, false)

	_, resp := aliasReqPath(t, s, http.MethodPut, "/api/config", `{"listen":":9000"}`)
	if resp["restart_required"] != true {
		t.Errorf("restart_required=%v want true", resp["restart_required"])
	}
	msg, _ := resp["message"].(string)
	if !strings.Contains(msg, "需重启") || !strings.Contains(msg, "listen") {
		t.Errorf("提示应点名需重启字段: %q", msg)
	}
	fields, ok := resp["restart_fields"].([]any)
	if !ok || len(fields) != 2 || fields[0] != "listen" {
		t.Errorf("restart_fields=%v want [listen upstash]", resp["restart_fields"])
	}
}

// TestConfigPutMixed 冷热混合：既要说明其余改动已生效，也要点名需重启的那项。
func TestConfigPutMixed(t *testing.T) {
	r := &fakeReloader{hot: []string{"api_key"}, cold: []string{"server"}}
	s, _ := newConfigTestServer(t, r, false)

	_, resp := aliasReqPath(t, s, http.MethodPut, "/api/config", `{"api_key":"n","server":{"read_timeout":"600s"}}`)
	if resp["restart_required"] != true {
		t.Errorf("restart_required=%v want true", resp["restart_required"])
	}
	msg, _ := resp["message"].(string)
	if !strings.Contains(msg, "已生效") || !strings.Contains(msg, "server") {
		t.Errorf("混合改动的提示应同时说明已生效与需重启项: %q", msg)
	}
}

// TestConfigPutReloadFailed 重载失败：文件已写、运行配置没变，绝不能说「已生效」。
func TestConfigPutReloadFailed(t *testing.T) {
	r := &fakeReloader{err: errBoom}
	s, _ := newConfigTestServer(t, r, false)

	_, resp := aliasReqPath(t, s, http.MethodPut, "/api/config", `{"api_key":"n"}`)
	if resp["reloaded"] != false {
		t.Errorf("reloaded=%v want false", resp["reloaded"])
	}
	if resp["restart_required"] != true {
		t.Errorf("重载失败必须提示需重启: %v", resp["restart_required"])
	}
	msg, _ := resp["message"].(string)
	if !strings.Contains(msg, "未生效") || strings.Contains(msg, "已生效") {
		t.Errorf("重载失败的提示不得声称已生效: %q", msg)
	}
}

// TestConfigPutNoReloader 独立面板模式：没有同进程网关，任何改动都要重启。
func TestConfigPutNoReloader(t *testing.T) {
	s, _ := newConfigTestServer(t, nil, false)

	_, resp := aliasReqPath(t, s, http.MethodPut, "/api/config", `{"api_key":"n"}`)
	if resp["reloaded"] != false || resp["restart_required"] != true {
		t.Errorf("独立面板模式应 reloaded=false restart_required=true，得到 %v/%v",
			resp["reloaded"], resp["restart_required"])
	}
	if msg, _ := resp["message"].(string); !strings.Contains(msg, "重启") {
		t.Errorf("提示应说明需重启: %q", msg)
	}
}

// TestConfigResetRestoresPreviousVersion 「恢复备份」必须能回到**上一次**保存前的内容，
// 而不是永远回到最初那份。这是本功能的实际动机：旧实现只在首次保存前备份一次，
// 改过几十次之后那个回退点早就没有意义。
func TestConfigResetRestoresPreviousVersion(t *testing.T) {
	r := &fakeReloader{hot: []string{"api_key"}}
	s, gwCfg := newConfigTestServer(t, r, true)

	// 连续保存两次：第二次保存前的内容（api_key=first）应成为最新备份。
	if _, resp := aliasReqPath(t, s, http.MethodPut, "/api/config", `{"api_key":"first"}`); resp["ok"] != true {
		t.Fatalf("第一次保存失败: %v", resp)
	}
	if _, resp := aliasReqPath(t, s, http.MethodPut, "/api/config", `{"api_key":"second"}`); resp["ok"] != true {
		t.Fatalf("第二次保存失败: %v", resp)
	}

	// 缺省恢复最新一份 = 上一次保存前 → "first"，而不是最初的 "k"。
	_, resp := aliasReqPath(t, s, http.MethodPost, "/api/config/reset", "")
	if resp["ok"] != true {
		t.Fatalf("恢复失败: %v", resp)
	}
	raw, err := os.ReadFile(gwCfg)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["api_key"] != "first" {
		t.Errorf("恢复最新备份应得到上一次保存前的值 first，实际 %v", doc["api_key"])
	}

	// 指定更早的一份：恢复动作本身也会留备份，所以序号会整体后移一位。
	// 按内容定位而不是写死序号（前端同样按时间/清单选，不依赖固定 index）。
	_, metaResp := aliasReqPath(t, s, http.MethodGet, "/api/config", "")
	meta, _ := metaResp["meta"].(map[string]any)
	list, _ := meta["backups"].([]any)
	if len(list) < 2 {
		t.Fatalf("应有至少 2 份备份，实际 %v", meta["backups"])
	}
	oldest, _ := list[len(list)-1].(map[string]any)
	oldestIdx, _ := oldest["index"].(float64)
	_, resp = aliasReqPath(t, s, http.MethodPost, "/api/config/reset",
		fmt.Sprintf(`{"backup":%d}`, int(oldestIdx)))
	if resp["ok"] != true {
		t.Fatalf("恢复最旧一份失败: %v", resp)
	}
	raw, err = os.ReadFile(gwCfg)
	if err != nil {
		t.Fatal(err)
	}
	doc = nil
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["api_key"] != "k" {
		t.Errorf("恢复最旧一份应得到最初的值 k，实际 %v", doc["api_key"])
	}
}

// TestConfigGetExposesBackups 前端要能列出可选备份，元信息必须带上清单。
func TestConfigGetExposesBackups(t *testing.T) {
	s, _ := newConfigTestServer(t, &fakeReloader{}, false)
	if _, resp := aliasReqPath(t, s, http.MethodPut, "/api/config", `{"api_key":"a"}`); resp["ok"] != true {
		t.Fatalf("保存失败: %v", resp)
	}
	_, resp := aliasReqPath(t, s, http.MethodGet, "/api/config", "")
	meta, _ := resp["meta"].(map[string]any)
	if meta == nil {
		t.Fatal("缺少 meta")
	}
	list, ok := meta["backups"].([]any)
	if !ok || len(list) != 1 {
		t.Fatalf("backups=%v want 1 份", meta["backups"])
	}
	first, _ := list[0].(map[string]any)
	if first["index"] != float64(1) {
		t.Errorf("最新备份 index=%v want 1", first["index"])
	}
	if p, _ := first["path"].(string); !strings.Contains(p, ".gui.bak.1") {
		t.Errorf("备份路径应带序号: %q", p)
	}
}

// TestConfigResetTriggersReload 恢复备份也必须触发热重载。
//
// 这是实际存在过的缺口：旧实现只写文件不通知网关，于是「恢复成功」的提示与实际
// 运行配置相反 —— 用户以为回滚了，网关仍在用新配置。
func TestConfigResetTriggersReload(t *testing.T) {
	r := &fakeReloader{hot: []string{"api_key"}}
	s, gwCfg := newConfigTestServer(t, r, true)

	// 先保存一次生成备份，再改坏文件，最后恢复。
	if _, resp := aliasReqPath(t, s, http.MethodPut, "/api/config", `{"api_key":"first"}`); resp["ok"] != true {
		t.Fatalf("前置保存失败: %v", resp)
	}
	if err := os.WriteFile(gwCfg, []byte(`{"api_key":"changed"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	r.calls = 0

	w, resp := aliasReqPath(t, s, http.MethodPost, "/api/config/reset", "")
	if w.Code != 200 {
		t.Fatalf("reset code=%d want 200 body=%s", w.Code, w.Body)
	}
	if r.calls != 1 {
		t.Errorf("恢复备份后应调用一次热重载，实际 %d 次", r.calls)
	}
	if resp["restart_required"] != false {
		t.Errorf("热字段恢复后不应提示重启: %v", resp["restart_required"])
	}
	raw, err := os.ReadFile(gwCfg)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("恢复后的文件不是合法 JSON: %v", err)
	}
	// 备份是「首次保存前」的原文件（此处是测试预置的 "k"），不是上一次保存的内容。
	if doc["api_key"] != "k" {
		t.Errorf("文件未恢复成首次保存前的备份: %v", doc["api_key"])
	}
}
