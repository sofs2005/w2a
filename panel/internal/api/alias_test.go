package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"workbuddy2api-gui/internal/config"
	"workbuddy2api-gui/internal/ops"
)

// newAliasServer 构造一个把网关 config.json 与别名文件都指向临时目录的 Server。
//
// 关键点：**别名路径不显式配置**（alias_file 留空），逼着测试走「从网关 config.json 的
// state_file 推导」这条生产路径——面板与网关必须算出同一个路径，这是本功能最容易
// 静默失效的地方（改完不生效、日志无异常），必须在测试里钉死。
func newAliasServer(t *testing.T, readOnly, dangerous bool) (*Server, string) {
	t.Helper()
	dir := t.TempDir()

	gwCfg := filepath.Join(dir, "config.json")
	stateFile := filepath.Join(dir, "data", "state.json")
	if err := os.WriteFile(gwCfg, []byte(`{"state_file":`+jsonString(stateFile)+`}`), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := config.Default()
	cfg.UI.Username = "admin"
	cfg.UI.Password = "pw123456"
	cfg.UI.TTL = 0
	cfg.UpstreamConfigFile = gwCfg
	cfg.UpstreamAliasFile = "" // 走推导
	cfg.ReadOnly = readOnly
	cfg.DangerousOps = dangerous

	svc := ops.New(cfg, nil, nil, nil)
	s := NewServer(cfg, svc, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), "test")
	return s, filepath.Join(filepath.Dir(stateFile), "model_aliases.json")
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// aliasReq 带会话 Cookie 请求别名端点，返回响应与解析后的 JSON。
// path 传空则默认为 /api/model-aliases。
func aliasReq(t *testing.T, s *Server, method, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	return aliasReqPath(t, s, method, "/api/model-aliases", body)
}

func aliasReqPath(t *testing.T, s *Server, method, path, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	token, ok, _ := s.sessions.Create("admin", "pw123456", s.cfg, "1.1.1.1")
	if !ok {
		t.Fatal("测试登录失败")
	}
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	var resp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	return w, resp
}

// TestAliasGetMissingFile 默认部署没有别名文件：不算错误，返回空表 + exists=false，
// 且提示语必须是「无需重启」（与 config.json 页的提示刻意不同，写错会误导运维）。
func TestAliasGetMissingFile(t *testing.T) {
	s, wantPath := newAliasServer(t, false, false)

	w, resp := aliasReq(t, s, http.MethodGet, "")
	if w.Code != 200 {
		t.Fatalf("code=%d want 200 body=%s", w.Code, w.Body)
	}
	got, ok := resp["aliases"].([]any)
	if !ok {
		t.Fatalf("aliases 应为数组: %v", resp["aliases"])
	}
	if len(got) != 0 {
		t.Errorf("文件不存在时应返回空表: %v", got)
	}
	meta, _ := resp["meta"].(map[string]any)
	if meta == nil {
		t.Fatal("缺少 meta")
	}
	if meta["exists"] != false {
		t.Errorf("exists=%v want false", meta["exists"])
	}
	if meta["path"] != wantPath {
		t.Errorf("推导路径=%v want %v（必须与网关 aliasFilePath 一致）", meta["path"], wantPath)
	}
	if note, _ := meta["restart_note"].(string); !strings.Contains(note, "无需重启") {
		t.Errorf("提示语应说明无需重启: %q", note)
	}
}

// TestAliasPutThenGet 写入 → 回读一致，并确认落盘格式与网关 internal/aliases 同构
// （顶层 aliases 数组、字段 name/cn/global），否则网关解析不到、面板却显示成功。
func TestAliasPutThenGet(t *testing.T) {
	s, wantPath := newAliasServer(t, false, false)

	body := `{"aliases":[
		{"name":" my-hidden ","cn":"","global":"gl-phys"},
		{"name":"my-dual","cn":"cn-phys","global":"gl-phys"}
	]}`
	w, resp := aliasReq(t, s, http.MethodPut, body)
	if w.Code != 200 {
		t.Fatalf("PUT code=%d want 200 body=%s", w.Code, w.Body)
	}
	if msg, _ := resp["message"].(string); !strings.Contains(msg, "无需重启") {
		t.Errorf("保存提示应说明热生效: %q", msg)
	}

	raw, err := os.ReadFile(wantPath)
	if err != nil {
		t.Fatalf("别名文件未写到推导路径 %s: %v", wantPath, err)
	}
	var doc struct {
		Aliases []struct {
			Name   string `json:"name"`
			CN     string `json:"cn"`
			Global string `json:"global"`
		} `json:"aliases"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("落盘格式非法（网关将无法解析）: %v\n%s", err, raw)
	}
	if len(doc.Aliases) != 2 {
		t.Fatalf("落盘条数=%d want 2: %s", len(doc.Aliases), raw)
	}
	// 首尾空白应被剥掉，否则网关侧的对外名带空格、请求永远匹配不上。
	if doc.Aliases[0].Name != "my-hidden" || doc.Aliases[0].CN != "" || doc.Aliases[0].Global != "gl-phys" {
		t.Errorf("第 1 条=%+v want {my-hidden, '', gl-phys}", doc.Aliases[0])
	}

	// 回读。
	w, resp = aliasReq(t, s, http.MethodGet, "")
	if w.Code != 200 {
		t.Fatalf("GET code=%d body=%s", w.Code, w.Body)
	}
	got, _ := resp["aliases"].([]any)
	if len(got) != 2 {
		t.Fatalf("回读条数=%d want 2", len(got))
	}
	meta, _ := resp["meta"].(map[string]any)
	if meta["exists"] != true {
		t.Errorf("写入后 exists=%v want true", meta["exists"])
	}
	// 首次写入必须留下备份（回退点），且备份内容是写入前的（此处文件本不存在 → 无备份）。
	// 故这里只断言「第二次写入后存在备份」由 TestAliasReset 覆盖。
}

// TestAliasPutRejectsInvalid 面板侧前置校验：网关对非法表是「静默保持旧表 + WARN」，
// 用户从界面看不出没生效，所以必须在这里拦下并给出可操作的原因。
func TestAliasPutRejectsInvalid(t *testing.T) {
	s, wantPath := newAliasServer(t, false, false)

	// 先写入一份合法表作为「原状」。
	if w, _ := aliasReq(t, s, http.MethodPut, `{"aliases":[{"name":"keep","cn":"cn-phys"}]}`); w.Code != 200 {
		t.Fatalf("预置合法表失败: %d %s", w.Code, w.Body)
	}
	before, _ := os.ReadFile(wantPath)

	cases := []struct{ name, body, wantMsg string }{
		{"两域同空", `{"aliases":[{"name":"x","cn":"","global":""}]}`, "不能同时为空"},
		{"对外名为空", `{"aliases":[{"name":"  ","cn":"cn-phys"}]}`, "不能为空"},
		{"含冒号", `{"aliases":[{"name":"cn:x","cn":"cn-phys"}]}`, "冒号"},
		{"重复对外名", `{"aliases":[{"name":"x","cn":"a"},{"name":"x","global":"b"}]}`, "重复"},
	}
	for _, c := range cases {
		w, resp := aliasReq(t, s, http.MethodPut, c.body)
		if w.Code != 400 {
			t.Errorf("%s: code=%d want 400 body=%s", c.name, w.Code, w.Body)
			continue
		}
		if msg, _ := resp["error"].(string); !strings.Contains(msg, c.wantMsg) {
			t.Errorf("%s: 错误信息 %q 未包含 %q", c.name, msg, c.wantMsg)
		}
		after, _ := os.ReadFile(wantPath)
		if string(after) != string(before) {
			t.Errorf("%s: 非法输入改动了磁盘文件", c.name)
		}
	}
}

// TestAliasPutReadOnly 只读模式关闭一切写操作。
func TestAliasPutReadOnly(t *testing.T) {
	s, _ := newAliasServer(t, true, false)
	w, resp := aliasReq(t, s, http.MethodPut, `{"aliases":[{"name":"x","cn":"cn-phys"}]}`)
	if w.Code != http.StatusForbidden {
		t.Fatalf("code=%d want 403 body=%s", w.Code, w.Body)
	}
	if resp["code"] != "read_only" {
		t.Errorf("code=%v want read_only", resp["code"])
	}
}

// TestAliasResetNeedsDangerousAndRestores 恢复备份：未开 dangerous_ops 一律拒绝；
// 开启后把文件还原成「首次保存前」的内容。
func TestAliasResetNeedsDangerousAndRestores(t *testing.T) {
	s, wantPath := newAliasServer(t, false, false)

	// 第一次写入：文件此前不存在 → 无备份可留（这是正确的，没有"原状"可言）。
	if w, _ := aliasReq(t, s, http.MethodPut, `{"aliases":[{"name":"v1","cn":"cn-v1"}]}`); w.Code != 200 {
		t.Fatalf("首次写入失败: %d %s", w.Code, w.Body)
	}
	// 第二次写入：此时会为「v1」留下备份。
	if w, _ := aliasReq(t, s, http.MethodPut, `{"aliases":[{"name":"v2","cn":"cn-v2"}]}`); w.Code != 200 {
		t.Fatalf("二次写入失败: %d %s", w.Code, w.Body)
	}

	w, resp := aliasReqPath(t, s, http.MethodPost, "/api/model-aliases/reset", "")
	if w.Code != http.StatusForbidden {
		t.Fatalf("未解锁高危操作时 code=%d want 403 body=%s", w.Code, w.Body)
	}
	if resp["code"] != "dangerous_ops_disabled" {
		t.Errorf("code=%v want dangerous_ops_disabled", resp["code"])
	}

	// 解锁后恢复。
	s.cfg.DangerousOps = true
	w, _ = aliasReqPath(t, s, http.MethodPost, "/api/model-aliases/reset", "")
	if w.Code != 200 {
		t.Fatalf("恢复失败: code=%d body=%s", w.Code, w.Body)
	}
	raw, _ := os.ReadFile(wantPath)
	if !strings.Contains(string(raw), "v1") || strings.Contains(string(raw), "v2") {
		t.Errorf("恢复后内容应为 v1 备份: %s", raw)
	}
}

// TestAliasPathExplicitOverride 部署布局特殊时用 alias_file 显式指定，
// 优先级高于从网关 config.json 推导。
func TestAliasPathExplicitOverride(t *testing.T) {
	s, _ := newAliasServer(t, false, false)
	custom := filepath.Join(t.TempDir(), "custom-aliases.json")
	s.cfg.UpstreamAliasFile = custom

	if w, _ := aliasReq(t, s, http.MethodPut, `{"aliases":[{"name":"x","cn":"cn-phys"}]}`); w.Code != 200 {
		t.Fatalf("写入失败: %d %s", w.Code, w.Body)
	}
	if _, err := os.Stat(custom); err != nil {
		t.Fatalf("应写入显式指定的路径: %v", err)
	}
}

// TestAliasPathUnderivable 网关未配 state_file（纯内存状态）→ 推导不出路径，
// 必须明确报错而不是静默写到一个"看起来对"的地方。
func TestAliasPathUnderivable(t *testing.T) {
	s, _ := newAliasServer(t, false, false)
	if err := os.WriteFile(s.cfg.UpstreamConfigFile, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	w, resp := aliasReq(t, s, http.MethodGet, "")
	if w.Code != 400 {
		t.Fatalf("code=%d want 400 body=%s", w.Code, w.Body)
	}
	if msg, _ := resp["error"].(string); !strings.Contains(msg, "state_file") {
		t.Errorf("错误信息应点明 state_file: %q", msg)
	}
}
