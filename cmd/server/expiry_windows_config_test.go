// pool.urgent_expiry / pool.critical_expiry 的配置面验收（24 小时积分到期优先层）。
//
// 选号语义（24h 层压过 72h 层、免费跨层优先）在 internal/pool/expiry_layers_test.go；
// 本文件只钉**配置面**三件事，都是「静默失效」型风险：
//  1. 默认值三处口径（Default() ⇄ config.example.json）不得漂移——运维首装抄的就是示例；
//  2. normalize 的宽容口径：空值/非法/非正回落默认，critical > urgent 钳到 urgent 并告警
//     （不 fail-fast：窗口写错最坏是退回改动前行为，不该挡住整个网关）；
//  3. 热重载真的推到运行中的池子（漏接 SetExpiryWindows 的表现是「日志说已生效，
//     池子还用旧窗口」，没有任何报错）；
//  4. **启动侧**注入（main 里那行曾是不可测的裸调用：默认值恰好等于配置默认值，
//     删掉它行为不变、没有任何用例变红，只有运维配了非默认窗口时才静默失效）。
package main

import (
	"bytes"
	"encoding/json"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/pool"
)

// TestExpiryWindowsDefaultsMatchExample 默认值锚点 + 示例配置口径一致。
func TestExpiryWindowsDefaultsMatchExample(t *testing.T) {
	c := Default()
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if c.UrgentExpiryDur != 72*time.Hour {
		t.Errorf("Default() urgent_expiry=%v want 72h", c.UrgentExpiryDur)
	}
	if c.CriticalExpiryDur != 24*time.Hour {
		t.Errorf("Default() critical_expiry=%v want 24h", c.CriticalExpiryDur)
	}

	raw, err := os.ReadFile("../../config.example.json")
	if err != nil {
		t.Skipf("示例配置不在（%v），跳过口径比对", err)
	}
	var doc struct {
		Pool struct {
			UrgentExpiry   *string `json:"urgent_expiry"`
			CriticalExpiry *string `json:"critical_expiry"`
		} `json:"pool"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("config.example.json 解析失败: %v", err)
	}
	// 指针判据：键缺席时运维抄不到这两个旋钮（缺键 ⇔ 抄示例，与 timeout_* 同款检查）。
	if doc.Pool.UrgentExpiry == nil {
		t.Fatal("config.example.json 缺少 pool.urgent_expiry（运维抄不到这个旋钮）")
	}
	if doc.Pool.CriticalExpiry == nil {
		t.Fatal("config.example.json 缺少 pool.critical_expiry（运维抄不到这个旋钮）")
	}
	u, err := time.ParseDuration(*doc.Pool.UrgentExpiry)
	if err != nil {
		t.Fatalf("示例 urgent_expiry=%q 不可解析: %v", *doc.Pool.UrgentExpiry, err)
	}
	cr, err := time.ParseDuration(*doc.Pool.CriticalExpiry)
	if err != nil {
		t.Fatalf("示例 critical_expiry=%q 不可解析: %v", *doc.Pool.CriticalExpiry, err)
	}
	if u != c.UrgentExpiryDur || cr != c.CriticalExpiryDur {
		t.Errorf("示例 (%v, %v) 与 Default() (%v, %v) 漂移", u, cr, c.UrgentExpiryDur, c.CriticalExpiryDur)
	}
}

// TestExpiryWindowsNormalizeFallbacks 归一规则逐条：空值/非法/非正 → 默认；
// critical > urgent → 钳平（并在日志里说明，否则运维看不出"3 天窗口配了等于没配"）。
func TestExpiryWindowsNormalizeFallbacks(t *testing.T) {
	cases := []struct {
		name             string
		urgent, critical string
		wantU, wantC     time.Duration
	}{
		{"空值回落默认", "", "", 72 * time.Hour, 24 * time.Hour},
		{"合法值原样", "48h", "12h", 48 * time.Hour, 12 * time.Hour},
		{"非法值回落默认", "abc", "xyz", 72 * time.Hour, 24 * time.Hour},
		{"零值回落默认", "0", "0", 72 * time.Hour, 24 * time.Hour},
		{"负值回落默认", "-1h", "2h", 72 * time.Hour, 2 * time.Hour},
		{"critical 超界钳到 urgent", "12h", "48h", 12 * time.Hour, 12 * time.Hour},
		{"同值合法", "6h", "6h", 6 * time.Hour, 6 * time.Hour},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := Default()
			cfg.Pool.UrgentExpiry = c.urgent
			cfg.Pool.CriticalExpiry = c.critical
			if err := cfg.normalize(); err != nil {
				t.Fatalf("normalize: %v（窗口键不得 fail-fast）", err)
			}
			if cfg.UrgentExpiryDur != c.wantU || cfg.CriticalExpiryDur != c.wantC {
				t.Fatalf("生效值 (%v, %v)，want (%v, %v)",
					cfg.UrgentExpiryDur, cfg.CriticalExpiryDur, c.wantU, c.wantC)
			}
			// 归一后的原文字符串必须与生效值一致（面板/默认配置回写读的是它）。
			if d, err := time.ParseDuration(cfg.Pool.UrgentExpiry); err != nil || d != cfg.UrgentExpiryDur {
				t.Errorf("urgent_expiry 原文 %q 与生效值 %v 不一致（err=%v）",
					cfg.Pool.UrgentExpiry, cfg.UrgentExpiryDur, err)
			}
			if d, err := time.ParseDuration(cfg.Pool.CriticalExpiry); err != nil || d != cfg.CriticalExpiryDur {
				t.Errorf("critical_expiry 原文 %q 与生效值 %v 不一致（err=%v）",
					cfg.Pool.CriticalExpiry, cfg.CriticalExpiryDur, err)
			}
		})
	}
}

// TestExpiryWindowsClampWarns 钳制必须留痕：分层语义反转是"配对了却行为相反"的
// 疑难问题，日志是运维唯一能发现的途径。
func TestExpiryWindowsClampWarns(t *testing.T) {
	var buf bytes.Buffer
	oldOut := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(oldOut) })

	cfg := Default()
	cfg.Pool.UrgentExpiry = "6h"
	cfg.Pool.CriticalExpiry = "48h"
	if err := cfg.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if cfg.CriticalExpiryDur != 6*time.Hour {
		t.Fatalf("critical 应钳到 urgent=6h，got %v", cfg.CriticalExpiryDur)
	}
	if out := buf.String(); !strings.Contains(out, "critical_expiry") || !strings.Contains(out, "WARN") {
		t.Errorf("钳制应打 WARN 且点明键名，日志=%q", out)
	}
}

// TestExpiryWindowsReloadPushesIntoLivePool 热重载链路：
// 写配置 → ReloadConfig → applyConfig → Pool.SetExpiryWindows → 池子生效。
// 用池子的真实生效值断言（而不是读配置字段），漏接线时这条必挂。
func TestExpiryWindowsReloadPushesIntoLivePool(t *testing.T) {
	path, tgt := newReloadFixture(t, `{"pool":{"urgent_expiry":"72h","critical_expiry":"24h"}}`)
	if u, c := tgt.pool.ExpiryWindows(); u != 72*time.Hour || c != 24*time.Hour {
		t.Fatalf("初始窗口 (%v, %v)，want (72h, 24h)", u, c)
	}

	writeConfig(t, path, `{"pool":{"urgent_expiry":"12h","critical_expiry":"4h"}}`)
	a := newConfigReloaderAdapter(path, tgt)
	hot, cold, err := a.ReloadConfig()
	if err != nil {
		t.Fatalf("ReloadConfig: %v", err)
	}
	if len(cold) != 0 {
		t.Errorf("pool 段应可热生效，cold=%v", cold)
	}
	var sawPool bool
	for _, f := range hot {
		if f == "pool" {
			sawPool = true
		}
	}
	if !sawPool {
		t.Fatalf("hot=%v 未包含 pool（改 urgent_expiry 应被识别为 pool 变更）", hot)
	}
	if u, c := tgt.pool.ExpiryWindows(); u != 12*time.Hour || c != 4*time.Hour {
		t.Fatalf("热重载未推到池子：生效窗口 (%v, %v)，want (12h, 4h)", u, c)
	}
}

// TestExpiryWindowsStartupInjection 启动侧注入（main.go 的装配链路）必须有测试覆盖。
//
// 为什么必须补这一条：启动侧那行注入原先只写在 main() 里，而 main() 不可测。它的
// 失效形态**完全静默**——配置默认值（72h/24h）恰好等于池子自己的默认值，删掉注入
// 行为一字不变、没有任何用例变红；只有运维把窗口配成非默认值（例如签到节奏变了
// 把 24h 改成 4h）时，才会表现为"配置写了却不生效"，且没有任何日志。
//
// 断言方式是直接看**池子的真实生效值**（而不是读配置字段），并把"注入前是默认值"
// 作为前置条件——否则池子本来就被别处设成目标值，这条断言会失去判别力。
// 判别力：把 applyPoolExpiryWindows 的函数体改成空实现，本用例立即变红（已实测）。
func TestExpiryWindowsStartupInjection(t *testing.T) {
	cfg := Default()
	cfg.Pool.UrgentExpiry = "12h"
	cfg.Pool.CriticalExpiry = "4h"
	if err := cfg.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if cfg.UrgentExpiryDur != 12*time.Hour || cfg.CriticalExpiryDur != 4*time.Hour {
		t.Fatalf("前置：生效配置 (%v, %v)，want (12h, 4h)", cfg.UrgentExpiryDur, cfg.CriticalExpiryDur)
	}

	p := pool.New("")
	t.Cleanup(p.Close)
	// 前置条件：新池是包内默认值（72h/24h），否则下面的断言证明不了注入生效。
	if u, c := p.ExpiryWindows(); u != 72*time.Hour || c != 24*time.Hour {
		t.Fatalf("前置：新池窗口 (%v, %v)，want 默认 (72h, 24h)", u, c)
	}

	applyPoolExpiryWindows(p, cfg)

	if u, c := p.ExpiryWindows(); u != 12*time.Hour || c != 4*time.Hour {
		t.Fatalf("启动侧未把配置推到池子：生效窗口 (%v, %v)，want (12h, 4h)"+
			"（漏接的表现是配置写了非默认值也不生效，且默认值下完全静默）", u, c)
	}
}
