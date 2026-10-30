// timeout_cooldown_clamp_test.go 钉住 pool.timeout_cooldown 的上限钳制（M-3 修复）。
//
// 缺陷形态（QA 报的 Minor）：degrade_cooldown 有 degrade_cooldown_max 兜底，
// timeout_cooldown 却只有下界（>0）没有上界。于是单位笔误 "60m"（本意 60s）会让
// 号被摘出池整整 1 小时——表现为「只慢了一下却整小时缺号」，而且全程没有日志。
//
// 第一版修法复用 degrade_cooldown_max 做上限，覆盖面不足：那个键默认 2h，比 "60m"
// 还大，**默认配置拦不住 1 小时级笔误**。拍板改为独立上限键：
//
//	pool.timeout_cooldown_max，默认 "10m"
//
// 归一规则（本文件逐条钉住）：
//   - 先按 timeout_cooldown_max 钳 timeout_cooldown，超限则钳 + WARN（不 fail-fast，
//     与 proxy 段的 WARN 回落口径一致）；
//   - 上限键本身空值 → 默认 10m（不告警）；非法/<=0 → 默认 10m + WARN 再钳
//     （"0" 不得成为绕过上限的后门，与 degrade_cooldown_max 的处理同口径）；
//   - 钳制只在 config 层做一次，pool 侧只消费已归一的值
//     （TestTimeoutCooldownClampStillReachesPool 用行为验证这一点）。
package main

import (
	"bytes"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
)

// defaultCooldownMaxStr / defaultCooldownMax 是 timeout_cooldown_max 的默认值，
// 测试里独立写一份字面量（不复用被测常量）：这样「常量被改成别的值」时用例会红，
// 而不是跟着一起漂。字符串形态单独写一份，因为「未钳制/回落」时字符串字段保留
// 配置里的原始写法（"10m"），只有**被钳制**的 timeout_cooldown 才归一为
// Duration.String()（"10m0s"）。
const defaultCooldownMaxStr = "10m"

var defaultCooldownMax = 10 * time.Minute

// writeClampConfig 落一个临时配置文件并返回路径（本文件独立，不复用其它用例的 helper）。
//
// 顺手把 WB2A_TIMEOUT_COOLDOWN_MAX 清空：本文件的断言都基于「配置文件里的值」，
// 若跑测试的机器上恰好导出了这个 env，用例会莫名其妙地红。清空（而不是删除）后
// applyEnv 的 `v != ""` 判断即视为未设置；需要测 env 的用例在 helper 之后 t.Setenv。
func writeClampConfig(t *testing.T, body string) string {
	t.Helper()
	t.Setenv("WB2A_TIMEOUT_COOLDOWN_MAX", "")
	fp := filepath.Join(t.TempDir(), "clamp.json")
	if err := os.WriteFile(fp, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return fp
}

// captureConfigLog 执行 fn 并返回其间写出的日志文本。
func captureConfigLog(fn func()) string {
	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(old)
	fn()
	return buf.String()
}

// loadClampConfig 加载配置并返回 (配置, 期间日志)。
func loadClampConfig(t *testing.T, fp string) (*Config, string) {
	t.Helper()
	var c *Config
	out := captureConfigLog(func() {
		var err error
		c, err = Load(fp)
		if err != nil {
			// 钳制口径是「钳正 + WARN」，任何超限/非法值都不许 fail-fast。
			t.Fatalf("Load(%s) 不得 fail-fast: %v", fp, err)
		}
	})
	return c, out
}

// TestTimeoutCooldownClampedToDefaultMax 默认上限（10m）就拦得住 "60m" 这类单位笔误。
//
// 这是本条缺陷的**原始场景**：手写 "60m" 本意 "60s"，默认配置下号被摘出池 1 小时。
// 独立上限键默认 10m 后，同一份配置钳到 10m 并留下 WARN。
func TestTimeoutCooldownClampedToDefaultMax(t *testing.T) {
	fp := writeClampConfig(t, `{"pool":{"timeout_cooldown":"60m"}}`)
	c, out := loadClampConfig(t, fp)

	if c.TimeoutCooldownDur != defaultCooldownMax {
		t.Fatalf("60m 应被钳到默认上限 10m, got %v", c.TimeoutCooldownDur)
	}
	// 生效值与字符串字段一并归一（「Pool.TimeoutCooldown 即生效值」的既有不变式）。
	if c.Pool.TimeoutCooldown != defaultCooldownMax.String() {
		t.Errorf("钳制后字符串字段应归一为上限值, got %q", c.Pool.TimeoutCooldown)
	}
	if c.TimeoutCooldownMaxD != defaultCooldownMax {
		t.Errorf("默认上限应为 10m, got %v", c.TimeoutCooldownMaxD)
	}
	// WARN 必须能让人对上「哪个键、原值多少、被钳成多少」。
	for _, want := range []string{
		"WARN: [config]",
		"pool.timeout_cooldown",
		"60m",
		"pool.timeout_cooldown_max",
		defaultCooldownMax.String(),
		"钳制",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("钳制日志缺少 %q（运维无从发现配置被改）: %s", want, out)
		}
	}
}

// TestTimeoutCooldownMaxExplicitNoClamp 上限配 10m 时，未超限的值原样保留且零 WARN。
//
// 同时钉边界：等于上限不钳（钳制条件是 d > max，不是 >=）。
func TestTimeoutCooldownMaxExplicitNoClamp(t *testing.T) {
	for _, v := range []string{"5m", "10m", "60s"} {
		fp := writeClampConfig(t, `{"pool":{"timeout_cooldown_max":"10m","timeout_cooldown":"`+v+`"}}`)
		c, out := loadClampConfig(t, fp)
		want, err := time.ParseDuration(v)
		if err != nil {
			t.Fatalf("用例自身写错: %v", err)
		}
		if c.TimeoutCooldownDur != want {
			t.Errorf("timeout_cooldown=%s 未超上限不得被钳制, got %v", v, c.TimeoutCooldownDur)
		}
		if c.Pool.TimeoutCooldown != v {
			t.Errorf("timeout_cooldown=%s 未钳制时字符串字段不应被改写, got %q", v, c.Pool.TimeoutCooldown)
		}
		if strings.Contains(out, "钳制") {
			t.Errorf("timeout_cooldown=%s 未超上限不得打钳制 WARN（误报会让人怀疑配置坏了）: %s", v, out)
		}
	}
}

// TestTimeoutCooldownMaxCustomValueRespected 上限可配：30m 时 60m → 30m。
func TestTimeoutCooldownMaxCustomValueRespected(t *testing.T) {
	fp := writeClampConfig(t, `{"pool":{"timeout_cooldown_max":"30m","timeout_cooldown":"60m"}}`)
	c, out := loadClampConfig(t, fp)
	if c.TimeoutCooldownDur != 30*time.Minute {
		t.Fatalf("应按配置的上限 30m 钳制, got %v", c.TimeoutCooldownDur)
	}
	if c.TimeoutCooldownMaxD != 30*time.Minute {
		t.Errorf("TimeoutCooldownMaxD 应为 30m, got %v", c.TimeoutCooldownMaxD)
	}
	for _, want := range []string{"pool.timeout_cooldown_max", "30m0s", "钳制"} {
		if !strings.Contains(out, want) {
			t.Errorf("钳制日志缺少 %q: %s", want, out)
		}
	}
}

// TestTimeoutCooldownMaxZeroOrInvalidFallsBack 上限键配 "0"/负值/非法串 → 回落默认
// 10m **再钳**，不得成为绕过上限的后门（与 degrade_cooldown_max 同口径）。
func TestTimeoutCooldownMaxZeroOrInvalidFallsBack(t *testing.T) {
	for _, maxV := range []string{"0", "-5m", "oops"} {
		fp := writeClampConfig(t, `{"pool":{"timeout_cooldown_max":"`+maxV+`","timeout_cooldown":"60m"}}`)
		c, out := loadClampConfig(t, fp)
		if c.TimeoutCooldownMaxD != defaultCooldownMax {
			t.Errorf("上限 %q 应回落默认 10m, got %v", maxV, c.TimeoutCooldownMaxD)
		}
		if c.Pool.TimeoutCooldownMax != defaultCooldownMaxStr {
			t.Errorf("上限 %q 回落时字符串字段也应归一为 10m, got %q", maxV, c.Pool.TimeoutCooldownMax)
		}
		if c.TimeoutCooldownDur != defaultCooldownMax {
			t.Errorf("上限 %q 时 60m 仍应被钳到 10m（0 不是绕过上限的后门）, got %v",
				maxV, c.TimeoutCooldownDur)
		}
		// 两条 WARN 都要在：上限键回落 + 出池时长钳制。
		for _, want := range []string{"pool.timeout_cooldown_max", "回落默认", "钳制"} {
			if !strings.Contains(out, want) {
				t.Errorf("上限 %q 的日志缺少 %q: %s", maxV, want, out)
			}
		}
	}
}

// TestTimeoutCooldownMaxEmptyUsesDefault 上限键显式写空串 → 默认 10m，且不告警
// （空串是「未设置」的同义写法，与其它「空值回落」键同口径）。
func TestTimeoutCooldownMaxEmptyUsesDefault(t *testing.T) {
	fp := writeClampConfig(t, `{"pool":{"timeout_cooldown_max":"","timeout_cooldown":"60m"}}`)
	c, out := loadClampConfig(t, fp)
	if c.TimeoutCooldownMaxD != defaultCooldownMax {
		t.Fatalf("空串应回落默认 10m, got %v", c.TimeoutCooldownMaxD)
	}
	if c.TimeoutCooldownDur != defaultCooldownMax {
		t.Errorf("空串上限下 60m 应被钳到 10m, got %v", c.TimeoutCooldownDur)
	}
	if strings.Contains(out, "非法或非正值") {
		t.Errorf("空串是「未设置」的同义写法，不应打「非法」WARN: %s", out)
	}
}

// TestTimeoutCooldownMaxEnvOverride WB2A_TIMEOUT_COOLDOWN_MAX 覆盖文件值并参与钳制。
func TestTimeoutCooldownMaxEnvOverride(t *testing.T) {
	fp := writeClampConfig(t, `{"pool":{"timeout_cooldown_max":"30m","timeout_cooldown":"60m"}}`)
	t.Setenv("WB2A_TIMEOUT_COOLDOWN_MAX", "15m")
	c, _ := loadClampConfig(t, fp)
	if c.TimeoutCooldownMaxD != 15*time.Minute {
		t.Fatalf("WB2A_TIMEOUT_COOLDOWN_MAX 未生效: %v", c.TimeoutCooldownMaxD)
	}
	if c.TimeoutCooldownDur != 15*time.Minute {
		t.Errorf("env 上限 15m 应参与钳制（60m → 15m）, got %v", c.TimeoutCooldownDur)
	}
}

// TestTimeoutCooldownMaxEnvInvalidIgnored 非法 env 值 WARN + 忽略（保留文件值），
// 不影响启动（照 WB2A_TIMEOUT_STREAK 的既有写法）。
func TestTimeoutCooldownMaxEnvInvalidIgnored(t *testing.T) {
	fp := writeClampConfig(t, `{"pool":{"timeout_cooldown_max":"20m","timeout_cooldown":"60m"}}`)
	t.Setenv("WB2A_TIMEOUT_COOLDOWN_MAX", "ten-minutes")
	c, out := loadClampConfig(t, fp)
	if c.TimeoutCooldownMaxD != 20*time.Minute {
		t.Errorf("非法 env 应被忽略（保留文件值 20m）, got %v", c.TimeoutCooldownMaxD)
	}
	if c.TimeoutCooldownDur != 20*time.Minute {
		t.Errorf("文件上限 20m 应参与钳制, got %v", c.TimeoutCooldownDur)
	}
	if !strings.Contains(out, "WB2A_TIMEOUT_COOLDOWN_MAX") {
		t.Errorf("非法 env 应打 WARN（否则配错了没人知道）: %s", out)
	}
}

// TestTimeoutCooldownClampStillReachesPool 钳制后的值必须真的注入池子（钳制只在
// config 层做，若注入路径用的是未钳制的值，钳制就是摆设）。这里用行为验证：
// 上限钳到 40ms 后，出池 40ms 即回池。
func TestTimeoutCooldownClampStillReachesPool(t *testing.T) {
	fp := writeClampConfig(t, `{"pool":{"timeout_streak":1,"timeout_cooldown":"1h","timeout_cooldown_max":"40ms"}}`)
	c, out := loadClampConfig(t, fp)
	if c.TimeoutCooldownDur != 40*time.Millisecond {
		t.Fatalf("1h 应被钳到 40ms, got %v", c.TimeoutCooldownDur)
	}
	if !strings.Contains(out, "钳制") {
		t.Errorf("钳制应打 WARN: %s", out)
	}
	// 装配面：main.go / configreload.go 用的就是 TimeoutCooldownDur，这里直接验它
	// 注入池子后的行为（钳制只在 config 层做，若注入路径用未钳制的值，钳制就是摆设）。
	p := pool.New("")
	defer p.Close()
	p.SetTimeout(c.Pool.TimeoutStreak, c.TimeoutCooldownDur)
	p.Add(&auth.Auth{UID: "u1"})
	if cooled := p.NoteTimeout("u1"); !cooled {
		t.Fatalf("阈 %d 时一次超时即应出池", c.Pool.TimeoutStreak)
	}
	if uids := p.AvailableUIDs(); len(uids) != 0 {
		t.Fatalf("出池期可用列表应为空, got %v", uids)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if uids := p.AvailableUIDs(); len(uids) == 1 && uids[0] == "u1" {
			return // 按钳制后的 40ms 到期回池
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("钳制后的出池时长未生效（2s 内未回池）")
}

// TestTimeoutCooldownMaxDefaultsMatchExample 新键的三处口径一致性（本文件负责
// cmd/server 常量 ⇄ config.example.json 这两处；pool 侧不参与钳制，只消费已归一值）：
//  1. Default() 的字符串与生效值都是 10m；
//  2. config.example.json 的 pool.timeout_cooldown_max 与之一致（运维首装抄的就是它）；
//  3. 示例配置走真实加载路径时不被回落覆盖（写 10m 就真是 10m）。
func TestTimeoutCooldownMaxDefaultsMatchExample(t *testing.T) {
	// 与 writeClampConfig 同口径：本用例断言的是「文件里的默认值」，先摘掉可能存在的
	// 同名 env，避免跑测试的机器导出过它而误红。
	t.Setenv("WB2A_TIMEOUT_COOLDOWN_MAX", "")
	c := Default()
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if c.Pool.TimeoutCooldownMax != defaultCooldownMaxStr {
		t.Errorf("Default().Pool.TimeoutCooldownMax=%q want %q",
			c.Pool.TimeoutCooldownMax, defaultCooldownMaxStr)
	}
	if c.TimeoutCooldownMaxD != defaultCooldownMax {
		t.Errorf("Default().TimeoutCooldownMaxD=%v want %v", c.TimeoutCooldownMaxD, defaultCooldownMax)
	}

	const examplePath = "../../config.example.json"
	raw, err := os.ReadFile(examplePath)
	if err != nil {
		t.Skipf("示例配置不在（%v），跳过口径比对", err)
	}
	var doc struct {
		Pool struct {
			TimeoutCooldownMax *string `json:"timeout_cooldown_max"`
		} `json:"pool"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("config.example.json 解析失败: %v", err)
	}
	if doc.Pool.TimeoutCooldownMax == nil {
		t.Fatal("config.example.json 缺少 pool.timeout_cooldown_max（运维抄不到这个旋钮）")
	}
	exampleDur, err := time.ParseDuration(*doc.Pool.TimeoutCooldownMax)
	if err != nil {
		t.Fatalf("示例 timeout_cooldown_max=%q 不可解析: %v", *doc.Pool.TimeoutCooldownMax, err)
	}
	if exampleDur != c.TimeoutCooldownMaxD {
		t.Errorf("示例 timeout_cooldown_max=%v 与 Default()=%v 漂移", exampleDur, c.TimeoutCooldownMaxD)
	}

	// 示例配置走真实加载路径：新键不得被回落/钳制改写。
	loaded, out := loadClampConfig(t, examplePath)
	if loaded.TimeoutCooldownMaxD != defaultCooldownMax {
		t.Errorf("示例配置加载后上限漂移: %v", loaded.TimeoutCooldownMaxD)
	}
	if loaded.Pool.TimeoutCooldownMax != defaultCooldownMaxStr {
		t.Errorf("示例配置加载后上限字符串漂移: %q", loaded.Pool.TimeoutCooldownMax)
	}
	if strings.Contains(out, "钳制") {
		t.Errorf("示例配置自身不得触发钳制 WARN（示例应是自洽的）: %s", out)
	}
}
