package main

import (
	"os"
	"path/filepath"
	"testing"
)

// 思考通道兜底（issue #135）：默认开；JSON 显式 false 与 env 都能关。
func TestReasoningAsContentDefaultAndOverride(t *testing.T) {
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.ReasoningAsContent == nil || !*c.ReasoningAsContent {
		t.Fatalf("默认应为 true，实际 %v", c.ReasoningAsContent)
	}

	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	if err := os.WriteFile(fp, []byte(`{"reasoning_as_content":false}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err = Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.ReasoningAsContent == nil || *c.ReasoningAsContent {
		t.Fatalf("JSON 显式 false 应生效，实际 %v", c.ReasoningAsContent)
	}

	t.Setenv("WB2A_REASONING_AS_CONTENT", "1")
	c, err = Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.ReasoningAsContent == nil || !*c.ReasoningAsContent {
		t.Fatalf("环境变量应覆盖 JSON，实际 %v", c.ReasoningAsContent)
	}

	t.Setenv("WB2A_REASONING_AS_CONTENT", "0")
	c, err = Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.ReasoningAsContent == nil || *c.ReasoningAsContent {
		t.Fatalf("环境变量 0 应关闭，实际 %v", c.ReasoningAsContent)
	}
}
