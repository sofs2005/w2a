package main

import (
	"os"
	"path/filepath"
	"testing"
)

// exampleConfigPath 仓库根目录的示例配置（go test 的工作目录是本包目录）。
const exampleConfigPath = "../../config.example.json"

// TestExampleConfigNewFieldsAndWiring 示例配置必须能被**真实加载路径**（Load → normalize）吃下去：
// 语法合法只是最低要求，字段类型/默认值/派生量都要过得了校验。
//
// 为什么值得单独测：config.example.json 是运维首装时唯一会抄的文件，也是 build.yml
// 里 `cp config.example.json config.json` 的构建输入。它坏掉的表现是「新部署起不来」，
// 而且 CI 的 build 作业不会跑 Load（只做 go build），所以要在测试里拦住。
func TestExampleConfigNewFieldsAndWiring(t *testing.T) {
	if _, err := os.Stat(exampleConfigPath); err != nil {
		t.Skipf("示例配置不在（%v），跳过", err)
	}
	c, err := Load(exampleConfigPath)
	if err != nil {
		t.Fatalf("config.example.json 不能通过 Load: %v", err)
	}

	// 三个新增键：存在且类型/取值正确。
	// gzip_body 必须是显式 false（opt-in 决策的锚点：gzip 是已知的 11-128 触发信号）。
	if c.Upstream.GzipBody {
		t.Error("示例配置的 upstream.gzip_body 必须是 false（opt-in，刻意与上游主干相反）")
	}
	if c.Features.SanitizeExtraFeatures == nil {
		t.Error("示例配置应显式写出 features.sanitize_extra_features（[] 而非缺失）")
	}
	if len(c.Features.SanitizeExtraFeatures) != 0 {
		t.Errorf("示例配置的 sanitize_extra_features 应为空表，got %v", c.Features.SanitizeExtraFeatures)
	}
	if c.Features.SanitizeExtraRewrites == nil {
		t.Error("示例配置应显式写出 features.sanitize_extra_rewrites（[] 而非缺失）")
	}
	if len(c.Features.SanitizeExtraRewrites) != 0 {
		t.Errorf("示例配置的 sanitize_extra_rewrites 应为空表，got %v", c.Features.SanitizeExtraRewrites)
	}

	// 新增字段不得破坏既有的派生量/默认值归一（抽查几处代表性字段）。
	if !c.Features.SanitizeBlacklistFingerprints {
		t.Error("示例配置应保持 sanitize_blacklist_fingerprints=true")
	}
	if c.SoftRateDur <= 0 || c.ServerReadTimeoutDur <= 0 {
		t.Errorf("派生时长未归一: soft_rate=%v read_timeout=%v", c.SoftRateDur, c.ServerReadTimeoutDur)
	}
	if c.Prompt.Mode != "passthrough" {
		t.Errorf("示例配置 prompt.mode=%q want passthrough", c.Prompt.Mode)
	}

	// 示例配置里的 api_key 是占位符 "test_key"：按本批新增的弱 key 告警规则，用它
	// 启动应当**告警**（这正是该特性的目标场景，不是缺陷）。钉住它避免有人把示例的
	// 占位符换成看起来像真 key 的串、从而让首装用户跳过告警。
	if notice := c.weakAPIKeyNotice(); notice == "" {
		t.Error("示例配置的占位符 api_key 应触发弱 key 告警")
	}

	// 装配路径也要能吃下去（buildUpstream 会做代理/超时/净化接线，任何一处对空值
	// 不宽容都会在这里炸）。
	up := buildUpstream(c)
	if up.GzipBody {
		t.Error("示例配置装配出的客户端不该开 gzip_body")
	}
	if !up.SanitizeFingerprints {
		t.Error("示例配置装配出的客户端应开启指纹脱敏")
	}
}

// TestSanitizeFieldsParseFromFile 新字段的**类型**契约：rewrites 是 [["原串","替换串"], …]。
// JSON 里写成别的形状（对象/单层数组）必须解析失败而不是静默丢弃——静默丢弃的表现是
// 「配置写了但不生效」，比报错难查得多。
func TestSanitizeFieldsParseFromFile(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	ok := write("ok.json", `{"features":{"sanitize_extra_features":[" NEW_TERM "],
		"sanitize_extra_rewrites":[["NEW_TERM","WorkBuddy"]]}}`)
	c, err := Load(ok)
	if err != nil {
		t.Fatalf("合法形态应能加载: %v", err)
	}
	// 注意：Load 不做词表清洗（那是 upstream.ConfigureSanitize 的职责），
	// 这里只断言原样进到 Config，避免两处都改导致「清洗两次」。
	if len(c.Features.SanitizeExtraFeatures) != 1 || c.Features.SanitizeExtraFeatures[0] != " NEW_TERM " {
		t.Errorf("特征表解析结果 = %q", c.Features.SanitizeExtraFeatures)
	}
	if len(c.Features.SanitizeExtraRewrites) != 1 ||
		c.Features.SanitizeExtraRewrites[0][0] != "NEW_TERM" ||
		c.Features.SanitizeExtraRewrites[0][1] != "WorkBuddy" {
		t.Errorf("替换对解析结果 = %v", c.Features.SanitizeExtraRewrites)
	}

	// 类型不符（rewrites 写成对象表）必须报错。
	bad := write("bad.json", `{"features":{"sanitize_extra_rewrites":{"a":"b"}}}`)
	if _, err := Load(bad); err == nil {
		t.Error("sanitize_extra_rewrites 的非法形状应加载失败")
	}

	// 缺省（老配置文件）：三键都缺席时必须保持旧行为（gzip 关、词表空）。
	legacy := write("legacy.json", `{"api_key":"0123456789abcdef"}`)
	c2, err := Load(legacy)
	if err != nil {
		t.Fatalf("老配置文件（无新键）应能加载: %v", err)
	}
	if c2.Upstream.GzipBody || len(c2.Features.SanitizeExtraFeatures) != 0 || len(c2.Features.SanitizeExtraRewrites) != 0 {
		t.Errorf("缺省语义变了: gzip=%v features=%v rewrites=%v",
			c2.Upstream.GzipBody, c2.Features.SanitizeExtraFeatures, c2.Features.SanitizeExtraRewrites)
	}
	// 未知字段仍然被忽略（非严格模式，既有语义不变）。
	unknown := write("unknown.json", `{"api_key":"0123456789abcdef","upstream":{"gzip_body":false,"bogus_key":1},
		"features":{"sanitize_extra_features":[],"future_flag":true}}`)
	if _, err := Load(unknown); err != nil {
		t.Errorf("未知字段不该导致加载失败: %v", err)
	}
}

// TestGzipBodyEnvOverride upstream.gzip_body 的环境变量逃生门（WB2A_GZIP_BODY）。
// 配置文件里关着时，容器部署仍可用环境变量临时打开做灰度验证。
func TestGzipBodyEnvOverride(t *testing.T) {
	t.Setenv("WB2A_GZIP_BODY", "true")
	dir := t.TempDir()
	p := filepath.Join(dir, "c.json")
	if err := os.WriteFile(p, []byte(`{"upstream":{"gzip_body":false}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Upstream.GzipBody {
		t.Error("WB2A_GZIP_BODY=true 应覆盖配置文件里的 false")
	}
	if up := buildUpstream(c); !up.GzipBody {
		t.Error("环境变量开启后 buildUpstream 应把 GzipBody 接到客户端上")
	}
	// 非法值静默忽略（保持配置值），不 panic、不改变默认。
	t.Setenv("WB2A_GZIP_BODY", "not-a-bool")
	c2, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c2.Upstream.GzipBody {
		t.Error("非法环境变量值应被忽略（保持配置文件里的 false）")
	}
}
