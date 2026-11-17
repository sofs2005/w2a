package upstream

import (
	"strings"
	"testing"
)

// TestHideDshProseRoleAware harness 名只在「harness 自己写的文本」里拿掉：
// system/developer 与工具描述替换，用户/助手话语不动；路径/标识符一律保护。
func TestHideDshProseRoleAware(t *testing.T) {
	body := []byte(`{"model":"glm-5.3","messages":[` +
		`{"role":"system","content":"You are running on DeepSeek Harness. Use DSH_HOME and ~/.dsh, see /root/DSH/Work/ and DSH.md."},` +
		`{"role":"user","content":"I use DeepSeek Harness daily."}],` +
		`"tools":[{"type":"function","function":{"name":"t","description":"A tool from DeepSeek Harness.","parameters":{"type":"object"}}}]}`)
	out := string(PrepareBodyOpt(body, true))

	if !strings.Contains(out, "You are running on the agent harness.") {
		t.Errorf("system 散文里的 harness 名未替换: %s", out)
	}
	// 路径/标识符/文件名必须原样保留。
	for _, keep := range []string{"DSH_HOME", "~/.dsh", "/root/DSH/Work/", "DSH.md"} {
		if !strings.Contains(out, keep) {
			t.Errorf("路径/标识符被误改，缺 %q: %s", keep, out)
		}
	}
	// 用户自己的话不动。
	if !strings.Contains(out, "I use DeepSeek Harness daily.") {
		t.Errorf("用户消息被改写: %s", out)
	}
	// 工具描述是 harness 自己写的 → 替换。
	if !strings.Contains(out, "A tool from the agent harness.") {
		t.Errorf("工具描述未替换: %s", out)
	}
}

// TestHideDshLines DSH 自己的固定句在任意角色里都按句改写（整句是 harness 写的）。
func TestHideDshLines(t *testing.T) {
	cases := []struct{ in, want string }{
		{"DSH itself decided", "the agent itself decided"},
		{"see DeepSeek Harness implementation checkout", "see agent implementation checkout"},
		{"via DeepSeek Harness Web GUI today", "via Web GUI today"},
	}
	for _, c := range cases {
		got := sanitizeText(c.in)
		if !strings.Contains(got, c.want) {
			t.Errorf("sanitizeText(%q)=%q want contains %q", c.in, got, c.want)
		}
	}
}

// TestHideDshProseIdempotent 重复净化不应叠加冠词或再次替换。
func TestHideDshProseIdempotent(t *testing.T) {
	once := hideDshProse("The DeepSeek Harness runs here")
	if strings.Contains(once, "the the") {
		t.Errorf("冠词叠加: %q", once)
	}
	twice := hideDshProse(once)
	if twice != once {
		t.Errorf("非幂等: %q -> %q", once, twice)
	}
}
