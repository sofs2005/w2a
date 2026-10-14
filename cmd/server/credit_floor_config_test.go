// credit_floor_config_test.go pool.credit_floor_cn / credit_floor_global 配置测试：
// 键缺席取默认（国内 100 / 国外 30）/ 显式覆盖 / 显式 0 关闭单域 / 负值钳 0 /
// 旧单值键 credit_floor 的向后兼容迁移。
package main

import (
	"os"
	"path/filepath"
	"testing"
)

// writeCfg 把 JSON 写进临时配置并 Load。
func writeCfg(t *testing.T, body string) *Config {
	t.Helper()
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	if err := os.WriteFile(fp, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(fp)
	if err != nil {
		t.Fatalf("Load(%s): %v", body, err)
	}
	return c
}

// TestCreditFloorDefault 键缺席 → 默认 100/30（保底默认生效，国内外分档）。
func TestCreditFloorDefault(t *testing.T) {
	c := Default()
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if c.Pool.CreditFloorCN != 100 || c.Pool.CreditFloorGlobal != 30 {
		t.Errorf("floors=%d/%d want 100/30 (defaults)",
			c.Pool.CreditFloorCN, c.Pool.CreditFloorGlobal)
	}
}

// TestCreditFloorDefaultFromEmptyFile 空 JSON 对象（pool 段整个缺席）同样取默认。
func TestCreditFloorDefaultFromEmptyFile(t *testing.T) {
	c := writeCfg(t, `{}`)
	if c.Pool.CreditFloorCN != 100 || c.Pool.CreditFloorGlobal != 30 {
		t.Errorf("floors=%d/%d want 100/30 (defaults)",
			c.Pool.CreditFloorCN, c.Pool.CreditFloorGlobal)
	}
}

// TestCreditFloorParsedFromFile 显式配置覆盖默认（两域各自独立解析）。
func TestCreditFloorParsedFromFile(t *testing.T) {
	c := writeCfg(t, `{"pool":{"credit_floor_cn":200,"credit_floor_global":50}}`)
	if c.Pool.CreditFloorCN != 200 || c.Pool.CreditFloorGlobal != 50 {
		t.Errorf("floors=%d/%d want 200/50", c.Pool.CreditFloorCN, c.Pool.CreditFloorGlobal)
	}
}

// TestCreditFloorExplicitZeroDisables 显式 0 = 关闭该域（必须与"键缺席取默认"区分开，
// 这是本次引入指针之外唯一需要靠 Default→Unmarshal 顺序保证的语义）。
func TestCreditFloorExplicitZeroDisables(t *testing.T) {
	c := writeCfg(t, `{"pool":{"credit_floor_cn":0}}`)
	if c.Pool.CreditFloorCN != 0 {
		t.Errorf("credit_floor_cn=%d want 0 (explicit off)", c.Pool.CreditFloorCN)
	}
	// 未写的另一域仍取默认。
	if c.Pool.CreditFloorGlobal != 30 {
		t.Errorf("credit_floor_global=%d want 30 (absent → default)", c.Pool.CreditFloorGlobal)
	}
}

// TestCreditFloorPartialOverride 只写一域，另一域保持默认。
func TestCreditFloorPartialOverride(t *testing.T) {
	c := writeCfg(t, `{"pool":{"credit_floor_global":80}}`)
	if c.Pool.CreditFloorCN != 100 {
		t.Errorf("credit_floor_cn=%d want 100 (absent → default)", c.Pool.CreditFloorCN)
	}
	if c.Pool.CreditFloorGlobal != 80 {
		t.Errorf("credit_floor_global=%d want 80", c.Pool.CreditFloorGlobal)
	}
}

// TestCreditFloorNegativeClamped 负值钳 0（非法即关闭，不报错：老配置误写不炸启动）。
func TestCreditFloorNegativeClamped(t *testing.T) {
	c := writeCfg(t, `{"pool":{"credit_floor_cn":-5,"credit_floor_global":-1}}`)
	if c.Pool.CreditFloorCN != 0 || c.Pool.CreditFloorGlobal != 0 {
		t.Errorf("floors=%d/%d want 0/0 (negative clamped)",
			c.Pool.CreditFloorCN, c.Pool.CreditFloorGlobal)
	}
}

// TestCreditFloorLegacyKeyMigrates 旧单值键 credit_floor 非 0 时迁移到两域：
// 老部署升级后保底语义不突变（此前它配了 100 就仍是 100，而不是被新默认顶成 100/30）。
func TestCreditFloorLegacyKeyMigrates(t *testing.T) {
	c := writeCfg(t, `{"pool":{"credit_floor":77}}`)
	if c.Pool.CreditFloorCN != 77 || c.Pool.CreditFloorGlobal != 77 {
		t.Errorf("floors=%d/%d want 77/77 (legacy key migrated)",
			c.Pool.CreditFloorCN, c.Pool.CreditFloorGlobal)
	}
	if c.Pool.CreditFloor != 0 {
		t.Errorf("credit_floor=%d want 0 (normalized off after migration)", c.Pool.CreditFloor)
	}
}

// TestCreditFloorLegacyKeyZeroKeepsDefaults 旧键写 0（旧默认=关闭）不覆盖新默认：
// 否则每份老配置升级后都会静默把保底关掉。
func TestCreditFloorLegacyKeyZeroKeepsDefaults(t *testing.T) {
	c := writeCfg(t, `{"pool":{"credit_floor":0}}`)
	if c.Pool.CreditFloorCN != 100 || c.Pool.CreditFloorGlobal != 30 {
		t.Errorf("floors=%d/%d want 100/30 (legacy 0 must not clobber defaults)",
			c.Pool.CreditFloorCN, c.Pool.CreditFloorGlobal)
	}
}

// TestCreditFloorLegacyKeyDoesNotClobberExplicit 新旧键并存时新键优先（尤其新键显式 0
// 关域，不能被非 0 旧键顶开）。
func TestCreditFloorLegacyKeyDoesNotClobberExplicit(t *testing.T) {
	c := writeCfg(t, `{"pool":{"credit_floor":77,"credit_floor_cn":0}}`)
	if c.Pool.CreditFloorCN != 0 {
		t.Errorf("credit_floor_cn=%d want 0 (explicit new key wins)", c.Pool.CreditFloorCN)
	}
	if c.Pool.CreditFloorGlobal != 77 {
		t.Errorf("credit_floor_global=%d want 77 (legacy fills the still-default realm)",
			c.Pool.CreditFloorGlobal)
	}
}

// TestCreditFloorLegacyKeyNegativeIgnored 旧键负值不迁移（钳 0 后语义等同"未配"，
// 不能把两域一起关掉）。
func TestCreditFloorLegacyKeyNegativeIgnored(t *testing.T) {
	c := writeCfg(t, `{"pool":{"credit_floor":-5}}`)
	if c.Pool.CreditFloorCN != 100 || c.Pool.CreditFloorGlobal != 30 {
		t.Errorf("floors=%d/%d want 100/30 (negative legacy key ignored)",
			c.Pool.CreditFloorCN, c.Pool.CreditFloorGlobal)
	}
}
