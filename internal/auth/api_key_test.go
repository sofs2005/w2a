package auth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestParseAPIKeyFlat 扁平形 api_key：key 同时当 Bearer，UID 为 anonymous_<末8>。
func TestParseAPIKeyFlat(t *testing.T) {
	raw := []byte(`{"api_key":"ck_abcdefgh12345678","realm":"cn"}`)
	a, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !a.IsAPIKey() {
		t.Fatal("应识别为 API Key 账号")
	}
	if a.AccessToken != "ck_abcdefgh12345678" {
		t.Errorf("accessToken=%q", a.AccessToken)
	}
	if a.UID != "anonymous_12345678" {
		t.Errorf("uid=%q want anonymous_12345678", a.UID)
	}
	if a.NeedsRefresh(0) {
		t.Error("key 账号不该需要续期")
	}
	// 显式 uid 优先。
	a2, _ := Parse([]byte(`{"apiKey":"k1","uid":"u9"}`))
	if a2.UID != "u9" {
		t.Errorf("显式 uid 被覆盖: %q", a2.UID)
	}
}

// TestParseAPIKeyNested 嵌套形顶层 apiKey 也认（插件形态扩展）。
func TestParseAPIKeyNested(t *testing.T) {
	raw := []byte(`{"auth":{"realm":"cn"},"account":{"nickname":"k"},"apiKey":"pt_1234567890ab"}`)
	a, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !a.IsAPIKey() || a.AccessToken != "pt_1234567890ab" {
		t.Errorf("nested key 未生效: key=%q access=%q", a.APIKey, a.AccessToken)
	}
}

// TestSaveAtomicKeepsAPIKey 续期落盘是整份重建，api_key 必须写回，否则账号下次
// 读盘就变成没有 key 的空壳。
func TestSaveAtomicKeepsAPIKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "key.json")
	if err := os.WriteFile(path, []byte(`{"api_key":"ck_zzzzzzzz"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	a, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	a.FilePath = path
	a.Nickname = "n"
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("SaveAtomic: %v", err)
	}
	out, _ := os.ReadFile(path)
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("落盘不是 JSON: %v", err)
	}
	if doc["api_key"] != "ck_zzzzzzzz" {
		t.Errorf("api_key 丢失: %s", out)
	}
	if !strings.Contains(string(out), "zzzzzzzz") {
		t.Errorf("key 内容缺失: %s", out)
	}
}
