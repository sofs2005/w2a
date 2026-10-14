package server

import (
	"reflect"
	"testing"

	"workbuddy2api/internal/auth"
)

// TestModelListCarriesListingCompat /v1/models 要能同时喂第三方目录客户端
// （issue #156：Magpie 只读 modalities.input 与 supported_reasoning_levels）。
//
// 我们的 supports_images / reasoning_supported_efforts 是既有约定（面板与别的客户端
// 在用），不能改；这里钉住「额外补的兼容字段与既有字段一一对应」，且**全表**成立
// ——以后加模型、换目录时不会漏。
func TestModelListCarriesListingCompat(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, `{"code":0,"data":{"models":[
			{"id":"img-effort","maxInputTokens":65536,"maxOutputTokens":8192,"supportsImages":true,
			 "reasoning":{"supportedEfforts":["low","high"],"defaultEffort":"high"}},
			{"id":"text-effort","maxInputTokens":65536,"maxOutputTokens":8192,"supportsImages":false,
			 "reasoning":{"supportedEfforts":["low"]}},
			{"id":"img-only","maxInputTokens":65536,"maxOutputTokens":8192,"supportsImages":true},
			{"id":"plain","maxInputTokens":65536,"maxOutputTokens":8192}
		],"agents":[{"name":"cli","models":["img-effort","text-effort","img-only","plain"]}]}}`, false
	})
	resetModelsCache()
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, GlobalEnabled: false})

	byID := map[string]map[string]any{}
	for _, m := range h.modelList() {
		if id, ok := m["id"].(string); ok {
			byID[id] = m
		}
	}

	// 图片能力 → modalities.input 含 "image"；无图片能力 → 不写该字段（不编造）。
	if mod, ok := byID["img-effort"]["modalities"].(map[string]any); !ok {
		t.Error("img-effort 应带 modalities")
	} else if in, _ := mod["input"].([]string); !reflect.DeepEqual(in, []string{"text", "image"}) {
		t.Errorf("img-effort modalities.input=%v want [text image]", in)
	}
	if _, ok := byID["text-effort"]["modalities"]; ok {
		t.Error("text-effort 无图片能力，不应带 modalities")
	}
	if _, ok := byID["img-only"]["modalities"]; !ok {
		t.Error("img-only 应带 modalities")
	}
	if _, ok := byID["plain"]["modalities"]; ok {
		t.Error("plain 无图片能力，不应带 modalities")
	}

	// 档位 → supported_reasoning_levels 逐项 {effort: …}，顺序与既有字段一致；
	// 无档位 → 不写该字段。
	wantLevels := []map[string]any{{"effort": "low"}, {"effort": "high"}}
	if lv, ok := byID["img-effort"]["supported_reasoning_levels"].([]map[string]any); !ok {
		t.Error("img-effort 应带 supported_reasoning_levels")
	} else if !reflect.DeepEqual(lv, wantLevels) {
		t.Errorf("img-effort supported_reasoning_levels=%v want %v", lv, wantLevels)
	}
	if _, ok := byID["plain"]["supported_reasoning_levels"]; ok {
		t.Error("plain 无档位，不应带 supported_reasoning_levels")
	}

	// 既有字段语义不变（只增不改）：supports_images 按「空值省略」只对有能力者写出，
	// reasoning_supported_efforts 同理。
	for _, id := range []string{"img-effort", "img-only"} {
		if byID[id]["supports_images"] != true {
			t.Errorf("%s 丢了既有字段 supports_images", id)
		}
	}
	for _, id := range []string{"text-effort", "plain"} {
		if _, ok := byID[id]["supports_images"]; ok {
			t.Errorf("%s 无图片能力，不应写 supports_images（空值省略）", id)
		}
	}
	if _, ok := byID["img-effort"]["reasoning_supported_efforts"]; !ok {
		t.Error("img-effort 丢了既有字段 reasoning_supported_efforts")
	}

	// 全表不变式：兼容字段与既有字段一一对应（以后加模型也不会漏）。
	for _, m := range h.modelList() {
		id, _ := m["id"].(string)
		_, hasMod := m["modalities"]
		if hasMod != (m["supports_images"] == true) {
			t.Errorf("%s: modalities 与 supports_images 不对应（%v vs %v）", id, hasMod, m["supports_images"])
		}
		_, hasLevels := m["supported_reasoning_levels"]
		if hasLevels != (m["reasoning_supported_efforts"] != nil) {
			t.Errorf("%s: supported_reasoning_levels 与 reasoning_supported_efforts 不对应", id)
		}
	}
}

// TestAddListingCompatUnit addListingCompat 的边界：非 map 输入不 panic、
// 空档位数组不写字段、effort 非字符串项被跳过（不产出空 effort 级别）。
func TestAddListingCompatUnit(t *testing.T) {
	t.Run("无能力不写字段", func(t *testing.T) {
		e := map[string]any{"id": "x"}
		addListingCompat(e)
		if _, ok := e["modalities"]; ok {
			t.Error("无 supports_images 不应写 modalities")
		}
		if _, ok := e["supported_reasoning_levels"]; ok {
			t.Error("无档位不应写 supported_reasoning_levels")
		}
	})
	t.Run("空档位数组不写字段", func(t *testing.T) {
		e := map[string]any{"reasoning_supported_efforts": []string{}}
		addListingCompat(e)
		if _, ok := e["supported_reasoning_levels"]; ok {
			t.Error("空档位数组不应写 supported_reasoning_levels")
		}
	})
	t.Run("非字符串档位项被跳过", func(t *testing.T) {
		e := map[string]any{"reasoning_supported_efforts": []any{"low", 42, "high"}}
		addListingCompat(e)
		lv, _ := e["supported_reasoning_levels"].([]map[string]any)
		want := []map[string]any{{"effort": "low"}, {"effort": "high"}}
		if !reflect.DeepEqual(lv, want) {
			t.Errorf("levels=%v want %v", lv, want)
		}
	})
	t.Run("supports_images 非 true 不写", func(t *testing.T) {
		// 只认布尔 true：字符串 "true" / 其他真值不触发（与既有字段同口径）。
		e := map[string]any{"supports_images": "true"}
		addListingCompat(e)
		if _, ok := e["modalities"]; ok {
			t.Error("supports_images 非布尔 true 不应写 modalities")
		}
	})
}
