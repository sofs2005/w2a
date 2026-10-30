// timeout_config_qa_test.go QA 独立验证（配置装配面）：pool.timeout_streak /
// pool.timeout_cooldown 的三处口径一致性与**热重载真的推到池子**。
//
// 与 timeout_config_test.go 的分工：那边覆盖解析/回落/env 的逐条规则，这里只补两个
// 最容易「静默失效」的横切面：
//  1. 三处口径（cmd/server 的 Default() ⇄ config.example.json）是否漂移
//     （pool 包常量那一半在 internal/pool/timeout_qa_test.go）；
//  2. 面板保存 → ReloadConfig → applyConfig → Pool.SetTimeout 这条链路是否真的把
//     新值推进了运行中的池子（漏接的表现是「配置改了没反应，且没有任何报错」）。
//
// 另含 M-3（pool.timeout_cooldown_max，默认 10m）的独立复验：默认钳制、"0"/负值/
// 非法不得成为后门、>= 边界不钳、env 生效、热重载同样注入钳制后的值——一律用
// 「注入池子后的真实出池时长」做行为验证，而不只看配置字段。
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

// TestQATimeoutConfigDefaultsMatchExample 默认值三处口径的 cmd 侧：
// Default() 的生效值必须与 config.example.json 写的一致（缺键 ⇔ 抄示例）。
func TestQATimeoutConfigDefaultsMatchExample(t *testing.T) {
	c := Default()
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if c.Pool.TimeoutStreak != 2 {
		t.Errorf("Default().Pool.TimeoutStreak=%d want 2", c.Pool.TimeoutStreak)
	}
	if c.TimeoutCooldownDur != 60*time.Second {
		t.Errorf("Default().TimeoutCooldownDur=%v want 60s", c.TimeoutCooldownDur)
	}

	const examplePath = "../../config.example.json"
	raw, err := os.ReadFile(examplePath)
	if err != nil {
		t.Skipf("示例配置不在（%v），跳过口径比对", err)
	}
	var doc struct {
		Pool struct {
			TimeoutStreak   *int   `json:"timeout_streak"`
			TimeoutCooldown string `json:"timeout_cooldown"`
		} `json:"pool"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("config.example.json 解析失败: %v", err)
	}
	if doc.Pool.TimeoutStreak == nil {
		t.Fatal("config.example.json 缺少 pool.timeout_streak")
	}
	if *doc.Pool.TimeoutStreak != c.Pool.TimeoutStreak {
		t.Errorf("示例 timeout_streak=%d 与 Default()=%d 漂移",
			*doc.Pool.TimeoutStreak, c.Pool.TimeoutStreak)
	}
	d, err := time.ParseDuration(doc.Pool.TimeoutCooldown)
	if err != nil {
		t.Fatalf("示例 timeout_cooldown=%q 不可解析: %v", doc.Pool.TimeoutCooldown, err)
	}
	if d != c.TimeoutCooldownDur {
		t.Errorf("示例 timeout_cooldown=%v 与 Default()=%v 漂移", d, c.TimeoutCooldownDur)
	}

	// 示例配置走真实加载路径时也不得被回落覆盖（写了 2/60s 就必须真是 2/60s）。
	loaded, err := Load(examplePath)
	if err != nil {
		t.Fatalf("示例配置不能通过 Load: %v", err)
	}
	if loaded.Pool.TimeoutStreak != 2 || loaded.TimeoutCooldownDur != 60*time.Second {
		t.Errorf("示例配置加载后生效值漂移: streak=%d cooldown=%v",
			loaded.Pool.TimeoutStreak, loaded.TimeoutCooldownDur)
	}
}

// TestQATimeoutReloadPushesIntoLivePool 热重载接线（第 8 条的核心）：
// 面板改 timeout_streak/timeout_cooldown → ReloadConfig → 运行中的池子立即按新值
// 行为（阈 1 一次即出池、出池 40ms 后自动回池）。
//
// 漏接 SetTimeout 的表现是：配置读到、日志说「已热生效」、池子还用旧值——没有任何
// 报错，只有行为不对。这条用例用**行为**（而不是读字段）来证明接线存在。
func TestQATimeoutReloadPushesIntoLivePool(t *testing.T) {
	path, tgt := newReloadFixture(t, `{"pool":{"timeout_streak":3,"timeout_cooldown":"5m"}}`)
	tgt.pool.Add(&auth.Auth{UID: "u1"})
	a := newConfigReloaderAdapter(path, tgt)

	// 改动前：阈 3，一次超时不出池。
	if cooled := tgt.pool.NoteTimeout("u1"); cooled {
		t.Fatal("precondition: 阈 3 时一次超时不该出池")
	}

	writeConfig(t, path, `{"pool":{"timeout_streak":1,"timeout_cooldown":"40ms"}}`)
	hot, cold, err := a.ReloadConfig()
	if err != nil {
		t.Fatalf("ReloadConfig: %v", err)
	}
	if len(cold) != 0 {
		t.Errorf("pool 段应可热生效, cold=%v", cold)
	}
	var sawPool bool
	for _, f := range hot {
		if f == "pool" {
			sawPool = true
		}
	}
	if !sawPool {
		t.Fatalf("hot=%v 未包含 pool（改 timeout_streak 应被识别为 pool 变更）", hot)
	}

	// 改动后：阈 1 → 一次超时立即出池（证明 streak 推到了池子）。
	if cooled := tgt.pool.NoteTimeout("u1"); !cooled {
		t.Fatal("热重载未生效：阈 1 时一次超时应立即出池")
	}
	if uids := tgt.pool.AvailableUIDs(); len(uids) != 0 {
		t.Fatalf("出池期可用列表应为空, got %v", uids)
	}
	// 出池 40ms 后回池（证明 cooldown 也推到了池子；若仍是旧的 5m 会超时失败）。
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if uids := tgt.pool.AvailableUIDs(); len(uids) == 1 && uids[0] == "u1" {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("热重载后的 timeout_cooldown=40ms 未生效（2s 内未回池）")
}

// ---- M-3（新键 pool.timeout_cooldown_max）的独立复验 ----
//
// 与工程师 timeout_cooldown_clamp_test.go 的分工：那边逐条钉归一规则（解析/WARN/
// 回落），这里只做**QA 侧的三件事**，且刻意用不同手法：
//  1. 用「钳制后的值注入池子后的真实出池时长」（/status cool_remaining）来证明钳制
//     不是只改了配置字段——配置对了但注入路径拿的是未钳制值，是这条修复最可能的
//     漏接形态；
//  2. "0"/负值/非法 不得成为绕过上限的后门（先回落 10m，**再**用 10m 去钳）；
//  3. 边界（等于上限不钳）与热重载路径（configreload.go 也要注入钳制后的值）。
//
// 上限 10m 在下面各用例里写成独立字面量（不复用被测常量）：常量被改成别的值时
// 用例应当红，而不是跟着一起漂。

// qaTimeoutDefaultMax 独立于被测常量的默认上限字面量。
const qaTimeoutDefaultMax = 10 * time.Minute

// qaWriteTimeoutConfig 落一份临时配置并返回路径。
//
// 顺手清空三个相关 env：本文件的断言都基于「配置文件里的值」，跑测试的机器若恰好
// 导出过同名 env，用例会莫名其妙红（清空而非删除，applyEnv 的 v != "" 即视为未设置）。
func qaWriteTimeoutConfig(t *testing.T, body string) string {
	t.Helper()
	for _, k := range []string{"WB2A_TIMEOUT_STREAK", "WB2A_TIMEOUT_COOLDOWN", "WB2A_TIMEOUT_COOLDOWN_MAX"} {
		t.Setenv(k, "")
	}
	fp := filepath.Join(t.TempDir(), "qa-timeout.json")
	if err := os.WriteFile(fp, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return fp
}

// qaLoadTimeoutConfig 走真实加载路径（Default → 文件 → env → normalize）。
func qaLoadTimeoutConfig(t *testing.T, fp string) (*Config, string) {
	t.Helper()
	var (
		c   *Config
		err error
	)
	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	c, err = Load(fp)
	log.SetOutput(old)
	if err != nil {
		t.Fatalf("Load(%s) 不得 fail-fast（钳制口径是「钳正 + WARN」）: %v", fp, err)
	}
	return c, buf.String()
}

// qaCoolRemainingSec 把配置注入池子并返回一次超时出池的**真实**剩余秒数。
//
// 这是行为验证的核心：池子不做上限钳制（钳制只在 config 层做一次），所以这里拿到的
// 数字就是「注入值」的直接证据——配置字段对了但注入路径用了未钳制值，这里立刻超界。
func qaCoolRemainingSec(t *testing.T, c *Config) int64 {
	t.Helper()
	p := pool.New("")
	defer p.Close()
	p.Add(&auth.Auth{UID: "u1"})
	p.SetTimeout(c.Pool.TimeoutStreak, c.TimeoutCooldownDur)
	if cooled := p.NoteTimeout("u1"); !cooled {
		t.Fatalf("阈 %d 时一次超时即应出池（precondition 不成立）", c.Pool.TimeoutStreak)
	}
	st, ok := p.Status("u1")
	if !ok || !st.Cooling {
		t.Fatalf("超时出池后应显示 Cooling: %+v ok=%v", st, ok)
	}
	return st.CoolRemaining
}

// TestQATimeoutCooldownClampIsWhatReachesThePool 默认上限（10m）拦得住 "60m" 笔误，
// 且**钳制后的值真的进了池子**（行为验证：出池剩余 ≈ 600s，不是 3600s）。
//
// 这是本条缺陷的原始场景：手写 "60m" 本意 "60s"，修复前号被摘出池整整 1 小时，
// 表现为「只慢了一下却整小时缺号」且全程无日志。
func TestQATimeoutCooldownClampIsWhatReachesThePool(t *testing.T) {
	fp := qaWriteTimeoutConfig(t, `{"pool":{"timeout_streak":1,"timeout_cooldown":"60m"}}`)
	c, out := qaLoadTimeoutConfig(t, fp)

	if c.TimeoutCooldownDur != qaTimeoutDefaultMax {
		t.Fatalf("60m 应被钳到默认上限 10m, got %v", c.TimeoutCooldownDur)
	}
	if c.TimeoutCooldownMaxD != qaTimeoutDefaultMax {
		t.Fatalf("缺省的上限键应生效为 10m, got %v", c.TimeoutCooldownMaxD)
	}
	// 运维必须能发现「配置被改了」：钳制要留 WARN，且能对上键名/原值/新值。
	for _, want := range []string{"WARN: [config]", "pool.timeout_cooldown", "60m", "钳制"} {
		if !strings.Contains(out, want) {
			t.Errorf("钳制日志缺少 %q（运维无从发现配置被改）: %s", want, out)
		}
	}

	// 行为验证：注入池子后真实出池 ≈ 10m。
	if got := qaCoolRemainingSec(t, c); got < 595 || got > 601 {
		t.Fatalf("钳制后的出池时长未真正注入池子：cool_remaining=%ds（want ≈600，未钳制会是 3600）", got)
	}

	// Default() 自身的口径（缺键时运维拿到的就是它）。
	d := Default()
	if err := d.normalize(); err != nil {
		t.Fatalf("Default().normalize(): %v", err)
	}
	if d.TimeoutCooldownMaxD != qaTimeoutDefaultMax {
		t.Errorf("Default() 的 timeout_cooldown_max 应为 10m, got %v", d.TimeoutCooldownMaxD)
	}
}

// TestQATimeoutCooldownMaxZeroOrBadIsNoBackdoor "0"/负值/非法 的上限键不得成为绕过
// 上限的后门：先回落默认 10m，**再**用 10m 去钳 "60m"。
//
// 这是本键最容易写错的语义（把 "0" 当成「不设上界」）：那样配 0 + "60m" 就能让
// 缺陷原样复现，等于给运维留了一个静默关掉保护的口子。
func TestQATimeoutCooldownMaxZeroOrBadIsNoBackdoor(t *testing.T) {
	for _, maxV := range []string{"0", "-5m", "10-minutes"} {
		fp := qaWriteTimeoutConfig(t, `{"pool":{"timeout_streak":1,`+
			`"timeout_cooldown_max":"`+maxV+`","timeout_cooldown":"60m"}}`)
		c, out := qaLoadTimeoutConfig(t, fp)

		if c.TimeoutCooldownMaxD != qaTimeoutDefaultMax {
			t.Errorf("上限 %q 应回落默认 10m, got %v", maxV, c.TimeoutCooldownMaxD)
		}
		if c.TimeoutCooldownDur != qaTimeoutDefaultMax {
			t.Errorf("上限 %q 时 60m 仍应被钳到 10m（不得成为后门）, got %v", maxV, c.TimeoutCooldownDur)
		}
		if !strings.Contains(out, "pool.timeout_cooldown_max") || !strings.Contains(out, "回落默认") {
			t.Errorf("上限 %q 的回落必须留 WARN（配错了没人知道）: %s", maxV, out)
		}
		// 行为面同样验一次：池子里拿到的必须是 10m。
		if got := qaCoolRemainingSec(t, c); got < 595 || got > 601 {
			t.Errorf("上限 %q 时注入池子的出池时长应为 ≈600s, got %d", maxV, got)
		}
	}
}

// TestQATimeoutCooldownBoundaryEqualsNotClamped 边界：等于上限**不**钳（条件是
// d > max，不是 >=），且不误报钳制 WARN。
//
// 判据不能只看数值（等于上限时钳与不钳结果相同），所以额外要求「字符串字段原样
// 保留」——只有真的走了钳制分支，代码才会把 Pool.TimeoutCooldown 改写成
// Duration.String()（"10m" → "10m0s"）。这条正是「>= 边界不钳」的可执行证据。
func TestQATimeoutCooldownBoundaryEqualsNotClamped(t *testing.T) {
	cases := []struct{ cooldown, max string }{
		{"10m", "10m"},  // 等于上限
		{"600s", "10m"}, // 同值不同写法
		{"60s", "10m"},  // 远小于上限
	}
	for _, tc := range cases {
		fp := qaWriteTimeoutConfig(t, `{"pool":{"timeout_streak":1,`+
			`"timeout_cooldown_max":"`+tc.max+`","timeout_cooldown":"`+tc.cooldown+`"}}`)
		c, out := qaLoadTimeoutConfig(t, fp)
		want, err := time.ParseDuration(tc.cooldown)
		if err != nil {
			t.Fatalf("用例自身写错: %v", err)
		}
		if c.TimeoutCooldownDur != want {
			t.Errorf("cooldown=%s max=%s 未超上限不得被钳制, got %v", tc.cooldown, tc.max, c.TimeoutCooldownDur)
		}
		if c.Pool.TimeoutCooldown != tc.cooldown {
			t.Errorf("cooldown=%s max=%s 未钳制时字符串字段应原样保留, got %q（钳制分支被误触发？）",
				tc.cooldown, tc.max, c.Pool.TimeoutCooldown)
		}
		if strings.Contains(out, "钳制") {
			t.Errorf("cooldown=%s max=%s 未超上限不得打钳制 WARN（误报让人怀疑配置坏了）: %s",
				tc.cooldown, tc.max, out)
		}
	}

	// 反向：刚超一点点（10m1s）就必须钳，且字符串字段归一为上限值。
	fp := qaWriteTimeoutConfig(t, `{"pool":{"timeout_streak":1,`+
		`"timeout_cooldown_max":"10m","timeout_cooldown":"10m1s"}}`)
	c, out := qaLoadTimeoutConfig(t, fp)
	if c.TimeoutCooldownDur != qaTimeoutDefaultMax {
		t.Fatalf("10m1s 超过上限 10m 应被钳制, got %v", c.TimeoutCooldownDur)
	}
	if c.Pool.TimeoutCooldown != qaTimeoutDefaultMax.String() {
		t.Errorf("钳制后字符串字段应归一为上限值, got %q", c.Pool.TimeoutCooldown)
	}
	if !strings.Contains(out, "钳制") {
		t.Errorf("10m1s 的钳制应打 WARN: %s", out)
	}
}

// TestQATimeoutCooldownMaxEnvApplied WB2A_TIMEOUT_COOLDOWN_MAX 生效并参与钳制；
// 非法 env 值 WARN + 忽略（保留文件值），不影响启动。
func TestQATimeoutCooldownMaxEnvApplied(t *testing.T) {
	fp := qaWriteTimeoutConfig(t, `{"pool":{"timeout_streak":1,`+
		`"timeout_cooldown_max":"30m","timeout_cooldown":"60m"}}`)

	t.Setenv("WB2A_TIMEOUT_COOLDOWN_MAX", "90s")
	c, _ := qaLoadTimeoutConfig(t, fp)
	if c.TimeoutCooldownMaxD != 90*time.Second {
		t.Fatalf("env 上限 90s 未生效, got %v", c.TimeoutCooldownMaxD)
	}
	if c.TimeoutCooldownDur != 90*time.Second {
		t.Fatalf("env 上限应参与钳制（60m → 90s）, got %v", c.TimeoutCooldownDur)
	}
	// 行为面：池子里拿到的是 90s，不是 30m/60m。
	if got := qaCoolRemainingSec(t, c); got < 85 || got > 91 {
		t.Fatalf("env 上限钳制后的出池时长未注入池子: cool_remaining=%ds（want ≈90）", got)
	}

	// 非法 env：忽略 + WARN，保留文件值（20m），仍然参与钳制。
	fp2 := qaWriteTimeoutConfig(t, `{"pool":{"timeout_streak":1,`+
		`"timeout_cooldown_max":"20m","timeout_cooldown":"60m"}}`)
	t.Setenv("WB2A_TIMEOUT_COOLDOWN_MAX", "not-a-duration")
	c2, out2 := qaLoadTimeoutConfig(t, fp2)
	if c2.TimeoutCooldownMaxD != 20*time.Minute {
		t.Errorf("非法 env 应被忽略（保留文件值 20m）, got %v", c2.TimeoutCooldownMaxD)
	}
	if c2.TimeoutCooldownDur != 20*time.Minute {
		t.Errorf("文件上限 20m 应参与钳制, got %v", c2.TimeoutCooldownDur)
	}
	if !strings.Contains(out2, "WB2A_TIMEOUT_COOLDOWN_MAX") {
		t.Errorf("非法 env 必须打 WARN（配错了没人知道）: %s", out2)
	}
}

// TestQATimeoutClampUsesOwnMaxNotDegradeMax M-3 的职责收窄在配置层的证据：
// degrade_cooldown_max 不得再钳 timeout_cooldown。
//
// 收窄前拿 degrade_cooldown_max 兜超时上界，于是「degrade 上限配 2m」会顺手把超时
// 出池压到 2m——调一个键改了两个行为。这里把 degrade 上限压到 2m 并要求超时仍按
// 自己的上限（10m）钳，两个方向的日志都要能区分。
func TestQATimeoutClampUsesOwnMaxNotDegradeMax(t *testing.T) {
	fp := qaWriteTimeoutConfig(t, `{"pool":{"timeout_streak":1,`+
		`"degrade_cooldown_max":"2m","timeout_cooldown":"60m"}}`)
	c, _ := qaLoadTimeoutConfig(t, fp)

	if c.TimeoutCooldownMaxD != qaTimeoutDefaultMax {
		t.Fatalf("超时上限应取自己的默认 10m（不受 degrade_cooldown_max=2m 影响）, got %v", c.TimeoutCooldownMaxD)
	}
	if c.TimeoutCooldownDur != qaTimeoutDefaultMax {
		t.Fatalf("60m 应被钳到 10m（不是 degrade 的 2m）, got %v", c.TimeoutCooldownDur)
	}
	if got := qaCoolRemainingSec(t, c); got < 595 || got > 601 {
		t.Fatalf("注入池子的出池时长应为 ≈600s（若被 degrade 上限钳会是 120s）, got %d", got)
	}
	// degrade 自己的上限仍生效（收窄 ≠ 失效）。
	if c.DegradeCooldownMaxD != 2*time.Minute {
		t.Errorf("degrade_cooldown_max 应仍解析为 2m, got %v", c.DegradeCooldownMaxD)
	}
}

// TestQATimeoutReloadInjectsClampedCooldown 热重载路径同样注入**钳制后**的值。
//
// configreload.go 的 applyConfig 用 cfg.Pool.TimeoutStreak / cfg.TimeoutCooldownDur
// 推给池子，而 cfg 是 Load 的产物（已归一/钳制）。漏接的表现是「面板把 60m 存进去，
// 热重载后号被摘 1 小时」——配置层钳了，热重载却拿了别的值。
func TestQATimeoutReloadInjectsClampedCooldown(t *testing.T) {
	t.Setenv("WB2A_TIMEOUT_COOLDOWN_MAX", "")
	path, tgt := newReloadFixture(t, `{"pool":{"timeout_streak":1,"timeout_cooldown":"60s"}}`)
	tgt.pool.Add(&auth.Auth{UID: "u1"})
	a := newConfigReloaderAdapter(path, tgt)

	// 面板把 timeout_cooldown 改成 60m（单位笔误），未配上限键。
	writeConfig(t, path, `{"pool":{"timeout_streak":1,"timeout_cooldown":"60m"}}`)
	hot, cold, err := a.ReloadConfig()
	if err != nil {
		t.Fatalf("ReloadConfig: %v", err)
	}
	if len(cold) != 0 {
		t.Fatalf("pool 段应可热生效, cold=%v", cold)
	}
	var sawPool bool
	for _, f := range hot {
		if f == "pool" {
			sawPool = true
		}
	}
	if !sawPool {
		t.Fatalf("hot=%v 未包含 pool（改 timeout_cooldown 应被识别为 pool 变更）", hot)
	}

	// 行为验证：热重载后出池 ≈ 600s（钳到 10m），而不是 3600s（未钳制）。
	if cooled := tgt.pool.NoteTimeout("u1"); !cooled {
		t.Fatal("热重载未生效：阈 1 时一次超时应立即出池")
	}
	st, _ := tgt.pool.Status("u1")
	if st.CoolRemaining < 595 || st.CoolRemaining > 601 {
		t.Fatalf("热重载注入的应是钳制后的 10m，got cool_remaining=%ds（3600 说明注入的是未钳制值）",
			st.CoolRemaining)
	}
}
