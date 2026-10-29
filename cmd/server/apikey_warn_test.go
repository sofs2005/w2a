package main

import (
	"strings"
	"testing"
)

// TestWeakAPIKeyNotice 启动期 api_key 弱值告警的判定口径。
//
// 背景：listen 默认绑 :7863（0.0.0.0），空 api_key 等于把网关连同全部账号池裸暴露；
// 而 "test_key" / "changeme" 这类占位符更隐蔽（运维看到 api_key 非空就放心了）。
// 但空 key 是 w2a **有意支持**的部署形态（纯内网 / 前置反代已鉴权），所以只告警不退出
// ——本用例钉住「该报的报、不该报的不报、空 key 不 panic」三条。
func TestWeakAPIKeyNotice(t *testing.T) {
	t.Run("占位值与过短 key 必须告警", func(t *testing.T) {
		for _, key := range []string{
			"test_key", "Test_Key", "your-api-key", "changeme", "sk-xxx", // 命中占位值集合（大小写不敏感）
			"short", "0123456789abcde", // 长度 < 16
		} {
			c := Default()
			c.APIKey = key
			notice := c.weakAPIKeyNotice()
			if notice == "" {
				t.Errorf("api_key=%q 应触发告警", key)
				continue
			}
			if !strings.HasPrefix(notice, "WARN: ") {
				t.Errorf("api_key=%q 的告警应以 WARN: 前缀开头（仓库日志风格）: %q", key, notice)
			}
			if !strings.Contains(notice, "占位符") && !strings.Contains(notice, "弱口令") {
				t.Errorf("api_key=%q 的告警应点明「疑似示例占位符」: %q", key, notice)
			}
		}
	})

	t.Run("正常 key 不告警", func(t *testing.T) {
		for _, key := range []string{
			"sk-bXQ3rT9mKpLz2vNc8YdA4wEf",           // WriteDefault 形态（同长度）
			"0123456789abcdef",                      // 恰好达到长度阈值
			"a-very-long-and-random-production-key", // 典型运维自定 key
		} {
			c := Default()
			c.APIKey = key
			if notice := c.weakAPIKeyNotice(); notice != "" {
				t.Errorf("api_key=%q 不该告警: %q", key, notice)
			}
		}
	})

	t.Run("空 key 告警且不 panic", func(t *testing.T) {
		c := Default()
		c.APIKey = ""
		c.Listen = ":7863"
		notice := c.weakAPIKeyNotice()
		if notice == "" {
			t.Fatal("空 api_key 必须告警（不鉴权 + 绑 0.0.0.0）")
		}
		if !strings.Contains(notice, "不鉴权") || !strings.Contains(notice, "0.0.0.0") {
			t.Errorf("空 key 告警应同时点明「不鉴权」与「绑 0.0.0.0」: %q", notice)
		}
		// 纯空白等同空（配置手改成 "   " 也是没设）。
		c.APIKey = "   "
		if c.weakAPIKeyNotice() == "" {
			t.Error("纯空白 api_key 应等同空值告警")
		}
		// 只告警不退出：warnWeakAPIKey 在空 key 下也不能 panic/退出。
		warnWeakAPIKey(c)
	})
}
