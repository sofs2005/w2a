// timeout_config_test.go pool.timeout_streak / pool.timeout_cooldown 配置测试：
// 默认值 / 文件覆盖 / 非法与越界值回落（**不 fail-fast**）/ env 覆盖 / 装配生效。
//
// 为什么这组配置值得单独钉：它是「连续上游超时 → 临时出池」的唯一旋钮。
// 两个键的处理口径与相邻的 degrade_* 刻意不同——degrade_* 非法值 fail-fast，
// 这里非法/<=0 一律回落默认 + WARN。回落写错的表现是「配置写了但不生效」或
// 「一个笔误把网关挡在启动门外」，两者都比缺省值难查得多，所以逐条钉死。
package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
)

// writeTempConfig 落一个临时配置文件并返回路径（同 state_flush_config_test 写法）。
func writeTempConfig(t *testing.T, body string) string {
	t.Helper()
	fp := filepath.Join(t.TempDir(), "c.json")
	if err := os.WriteFile(fp, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return fp
}

// TestTimeoutDefaults 键缺席 → 默认 2 次 / 60s（与 pool 包 defaultTimeout* 同口径）。
func TestTimeoutDefaults(t *testing.T) {
	c := Default()
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if c.Pool.TimeoutStreak != 2 {
		t.Errorf("timeout_streak=%d want 2 (default)", c.Pool.TimeoutStreak)
	}
	if c.TimeoutCooldownDur != 60*time.Second {
		t.Errorf("timeout_cooldown=%v want 60s (default)", c.TimeoutCooldownDur)
	}
}

// TestTimeoutParsedFromFile 显式配置覆盖默认。
func TestTimeoutParsedFromFile(t *testing.T) {
	fp := writeTempConfig(t, `{"pool":{"timeout_streak":5,"timeout_cooldown":"90s"}}`)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Pool.TimeoutStreak != 5 {
		t.Errorf("timeout_streak=%d want 5", c.Pool.TimeoutStreak)
	}
	if c.TimeoutCooldownDur != 90*time.Second {
		t.Errorf("timeout_cooldown=%v want 90s", c.TimeoutCooldownDur)
	}
}

// TestTimeoutBadCooldownFallsBack 非法时长**不 fail-fast**，回落默认 60s。
// 与 degrade_cooldown 的 fail-fast 不同：这个参数坏掉最坏是「超时号多挨一轮」，
// 不值得让整个网关起不来。
func TestTimeoutBadCooldownFallsBack(t *testing.T) {
	fp := writeTempConfig(t, `{"pool":{"timeout_cooldown":"oops"}}`)
	c, err := Load(fp)
	if err != nil {
		t.Fatalf("非法 timeout_cooldown 不得 fail-fast: %v", err)
	}
	if c.TimeoutCooldownDur != 60*time.Second {
		t.Errorf("非法值应回落 60s, got %v", c.TimeoutCooldownDur)
	}
	if c.Pool.TimeoutCooldown != "60s" {
		t.Errorf("回落时字符串也应归一为 60s, got %q", c.Pool.TimeoutCooldown)
	}
}

// TestTimeoutZeroAndNegativeCooldownFallBack "0" / 负值同样回落默认：
// 0 会让出池瞬间到期（等于没出池），负值无合理语义——两者都按非法处理。
func TestTimeoutZeroAndNegativeCooldownFallBack(t *testing.T) {
	for _, v := range []string{"0", "-5s"} {
		fp := writeTempConfig(t, `{"pool":{"timeout_cooldown":"`+v+`"}}`)
		c, err := Load(fp)
		if err != nil {
			t.Fatalf("timeout_cooldown=%q 不得 fail-fast: %v", v, err)
		}
		if c.TimeoutCooldownDur != 60*time.Second {
			t.Errorf("timeout_cooldown=%q 应回落 60s, got %v", v, c.TimeoutCooldownDur)
		}
	}
}

// TestTimeoutEmptyCooldownFallsBack 显式空串回落默认 60s。
func TestTimeoutEmptyCooldownFallsBack(t *testing.T) {
	fp := writeTempConfig(t, `{"pool":{"timeout_cooldown":""}}`)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.TimeoutCooldownDur != 60*time.Second {
		t.Errorf("空串应回落 60s, got %v", c.TimeoutCooldownDur)
	}
}

// TestTimeoutStreakNonPositiveFallsBack 次数 <=0 无合理语义 → 回落默认 2
// （阈 0/负数会让「任何一次超时都出池」，把单次抖动也罚掉）。
func TestTimeoutStreakNonPositiveFallsBack(t *testing.T) {
	for _, v := range []string{"0", "-3"} {
		fp := writeTempConfig(t, `{"pool":{"timeout_streak":`+v+`}}`)
		c, err := Load(fp)
		if err != nil {
			t.Fatalf("timeout_streak=%s 不得 fail-fast: %v", v, err)
		}
		if c.Pool.TimeoutStreak != 2 {
			t.Errorf("timeout_streak=%s 应回落 2, got %d", v, c.Pool.TimeoutStreak)
		}
	}
}

// TestTimeoutEnvOverride WB2A_TIMEOUT_STREAK / WB2A_TIMEOUT_COOLDOWN 覆盖文件值
// （与其余 pool 参数同序：Default → Unmarshal → applyEnv → normalize）。
func TestTimeoutEnvOverride(t *testing.T) {
	fp := writeTempConfig(t, `{"pool":{"timeout_streak":2,"timeout_cooldown":"60s"}}`)
	t.Setenv("WB2A_TIMEOUT_STREAK", "4")
	t.Setenv("WB2A_TIMEOUT_COOLDOWN", "45s")
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Pool.TimeoutStreak != 4 {
		t.Errorf("WB2A_TIMEOUT_STREAK 未生效: %d", c.Pool.TimeoutStreak)
	}
	if c.TimeoutCooldownDur != 45*time.Second {
		t.Errorf("WB2A_TIMEOUT_COOLDOWN 未生效: %v", c.TimeoutCooldownDur)
	}
}

// TestTimeoutEnvBadStreakIgnored 非法整数 env 被忽略（保留文件值），不影响启动。
func TestTimeoutEnvBadStreakIgnored(t *testing.T) {
	fp := writeTempConfig(t, `{"pool":{"timeout_streak":3}}`)
	t.Setenv("WB2A_TIMEOUT_STREAK", "abc")
	c, err := Load(fp)
	if err != nil {
		t.Fatalf("非法 env 不得 fail-fast: %v", err)
	}
	if c.Pool.TimeoutStreak != 3 {
		t.Errorf("非法 env 应被忽略（保留文件值 3）, got %d", c.Pool.TimeoutStreak)
	}
}

// TestTimeoutConfigWiresIntoPool 装配生效：config 解析出的两个值经 SetTimeout 注入
// 池后，行为与配置一致（阈 1 + 30ms → 一次超时即出池，快速到期回池）。
//
// 为什么测这条：SetTimeout 的调用点在 main.go / configreload.go 两处，漏接的表现是
// 「配置改了没反应」（且没有任何报错）。这里直接走 config → pool 的装配语义。
func TestTimeoutConfigWiresIntoPool(t *testing.T) {
	fp := writeTempConfig(t, `{"pool":{"timeout_streak":1,"timeout_cooldown":"40ms"}}`)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	p := pool.New("")
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
		if uids := p.AvailableUIDs(); len(uids) == 1 {
			return // 按配置的 40ms 到期回池
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("按配置时长到期后账号应自动回池（2s 内）")
}
