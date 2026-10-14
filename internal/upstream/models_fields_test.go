package upstream

import (
	"net/http"
	"testing"

	"workbuddy2api/internal/auth"
)

// authStub 测试用最小账号（CN realm 缺省）。
func authStub() *auth.Auth {
	return &auth.Auth{AccessToken: "at", UID: "u1"}
}

// TestFetchModelsParsesDefaultEffort P1：上游 reasoning.defaultEffort 应落到
// ModelInfo.DefaultEffort。
func TestFetchModelsParsesDefaultEffort(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"data":{"models":[
			{"id":"deepseek-v4-flash","name":"DS","maxInputTokens":131072,"maxOutputTokens":8192,
			 "reasoning":{"defaultEffort":"high","supportedEfforts":["low","high"]}}
		],"agents":[{"name":"cli","models":["deepseek-v4-flash"]}]}}`), nil
	})
	infos, err := c.FetchModels(authStub())
	if err != nil {
		t.Fatalf("fetch models: %v", err)
	}
	if len(infos) != 1 {
		t.Fatalf("infos=%+v", infos)
	}
	if infos[0].DefaultEffort != "high" {
		t.Errorf("DefaultEffort=%q want high", infos[0].DefaultEffort)
	}
}

// TestFetchModelsParsesSupportsImages P1：顶层 supportsImages 应落到
// ModelInfo.SupportsImages。
func TestFetchModelsParsesSupportsImages(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"data":{"models":[
			{"id":"glm-5v-turbo","name":"GLM-V","maxInputTokens":131072,"maxOutputTokens":8192,
			 "supportsImages":true,"reasoning":{"supportedEfforts":["low","high"]}}
		],"agents":[{"name":"cli","models":["glm-5v-turbo"]}]}}`), nil
	})
	infos, err := c.FetchModels(authStub())
	if err != nil {
		t.Fatalf("fetch models: %v", err)
	}
	if len(infos) != 1 {
		t.Fatalf("infos=%+v", infos)
	}
	if !infos[0].SupportsImages {
		t.Errorf("SupportsImages=false want true")
	}
}

// TestFetchModelsDefaultEffortMissing P1：reasoning 里没有 defaultEffort → 字段为空串。
func TestFetchModelsDefaultEffortMissing(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"data":{"models":[
			{"id":"kimi-k2.7","name":"Kimi","maxInputTokens":131072,"maxOutputTokens":8192,
			 "reasoning":{"supportedEfforts":["low","high"]}}
		],"agents":[{"name":"cli","models":["kimi-k2.7"]}]}}`), nil
	})
	infos, err := c.FetchModels(authStub())
	if err != nil {
		t.Fatalf("fetch models: %v", err)
	}
	if len(infos) != 1 {
		t.Fatalf("infos=%+v", infos)
	}
	if infos[0].DefaultEffort != "" {
		t.Errorf("DefaultEffort=%q want empty", infos[0].DefaultEffort)
	}
}

// TestFetchModelsSupportsImagesMissing P1：上游不返回 supportsImages → false。
func TestFetchModelsSupportsImagesMissing(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"data":{"models":[
			{"id":"kimi-k2.7","name":"Kimi","maxInputTokens":131072,"maxOutputTokens":8192}
		],"agents":[{"name":"cli","models":["kimi-k2.7"]}]}}`), nil
	})
	infos, err := c.FetchModels(authStub())
	if err != nil {
		t.Fatalf("fetch models: %v", err)
	}
	if len(infos) != 1 {
		t.Fatalf("infos=%+v", infos)
	}
	if infos[0].SupportsImages {
		t.Errorf("SupportsImages=true want false (field absent)")
	}
}

// --- P2：非对话模型过滤 ---

// TestFetchModelsFiltersNonChatPrefixes P2：nes-/completion-/codewise- 前缀过滤。
func TestFetchModelsFiltersNonChatPrefixes(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"data":{"models":[
			{"id":"nes-test-model","name":"NES","maxInputTokens":8192,"maxOutputTokens":8192},
			{"id":"completion-something","name":"Comp","maxInputTokens":8192,"maxOutputTokens":8192},
			{"id":"codewise-x","name":"CW","maxInputTokens":8192,"maxOutputTokens":8192},
			{"id":"deepseek-v4-flash","name":"DS","maxInputTokens":131072,"maxOutputTokens":8192}
		],"agents":[{"name":"cli","models":["nes-test-model","completion-something","codewise-x","deepseek-v4-flash"]}]}}`), nil
	})
	infos, err := c.FetchModels(authStub())
	if err != nil {
		t.Fatalf("fetch models: %v", err)
	}
	if len(infos) != 1 || infos[0].ID != "deepseek-v4-flash" {
		t.Fatalf("expected only deepseek-v4-flash, got %+v", infos)
	}
}

// TestFetchModelsFiltersTinyOutput P2：maxOutputTokens≤256 过滤（边界 256 被过滤，257 保留）。
func TestFetchModelsFiltersTinyOutput(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"data":{"models":[
			{"id":"tiny-128","name":"T128","maxInputTokens":8192,"maxOutputTokens":128},
			{"id":"tiny-256","name":"T256","maxInputTokens":8192,"maxOutputTokens":256},
			{"id":"small-257","name":"S257","maxInputTokens":8192,"maxOutputTokens":257}
		],"agents":[{"name":"cli","models":["tiny-128","tiny-256","small-257"]}]}}`), nil
	})
	infos, err := c.FetchModels(authStub())
	if err != nil {
		t.Fatalf("fetch models: %v", err)
	}
	if len(infos) != 1 || infos[0].ID != "small-257" {
		t.Fatalf("expected only small-257 (257>256), got %+v", infos)
	}
}

// TestFetchModelsFiltersTextToImage P2：tags 含 text-to-image 过滤。
func TestFetchModelsFiltersTextToImage(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"data":{"models":[
			{"id":"img-gen","name":"IMG","maxInputTokens":8192,"maxOutputTokens":8192,"tags":["text-to-image","chat"]},
			{"id":"glm-5.2","name":"GLM","maxInputTokens":131072,"maxOutputTokens":8192,"tags":["chat"]}
		],"agents":[{"name":"cli","models":["img-gen","glm-5.2"]}]}}`), nil
	})
	infos, err := c.FetchModels(authStub())
	if err != nil {
		t.Fatalf("fetch models: %v", err)
	}
	if len(infos) != 1 || infos[0].ID != "glm-5.2" {
		t.Fatalf("expected only glm-5.2 (no text-to-image), got %+v", infos)
	}
}

// TestNonChatModelGenerationTags 生成类 tag 全谱过滤（fork linguo2625469 7b24e22）：
// 图片 / 视频生成模型都必须挡在对话目录外——旧规则只认 text-to-image，视频模型会
// 混进目录、被选中后报 code=11102。
//
// 表里同时放**反向对照**（"chat"/"reasoning" 等对话类 tag 不得误伤）与"名字像生成、
// tag 是对话"的条目：若把判据写成 id 前缀或子串匹配，这些行会红。
func TestNonChatModelGenerationTags(t *testing.T) {
	for _, c := range []struct {
		name string
		id   string
		tags []string
		want bool
	}{
		{"text-to-image 仍拦", "img-gen", []string{"text-to-image", "chat"}, true},
		{"image-to-image 拦", "gpt-image-1", []string{"image-to-image"}, true},
		{"text-to-video 拦", "seedance-2.5", []string{"text-to-video"}, true},
		{"image-to-video 拦", "seedance-2.5-i2v", []string{"image-to-video"}, true},
		{"生成 tag 混在对话 tag 中仍拦", "seedance-2.5", []string{"chat", "text-to-video"}, true},
		{"纯对话 tag 不拦", "glm-5.2", []string{"chat"}, false},
		{"推理 tag 不拦", "deepseek-v4.1-flash", []string{"chat", "reasoning"}, false},
		{"无 tag 不拦", "gpt-5.4", nil, false},
		{"名字含 video 但 tag 是对话 → 不拦", "video-chat-1", []string{"chat"}, false},
		{"tag 大小写不匹配不拦（上游为小写字面量，不做模糊匹配）", "x", []string{"Text-To-Image"}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := nonChatModel(c.id, 8192, c.tags); got != c.want {
				t.Errorf("nonChatModel(%q, 8192, %v) = %v, want %v", c.id, c.tags, got, c.want)
			}
		})
	}
}

// TestFetchModelsFiltersVideoGenTags 端到端：上游目录带 seedance 视频模型时，
// FetchModels 的并集结果不得含它（本函数 CN/global 共用，CN 侧同样生效）。
func TestFetchModelsFiltersVideoGenTags(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"data":{"models":[
			{"id":"seedance-2.5","name":"Seedance","maxInputTokens":8192,"maxOutputTokens":8192,"tags":["text-to-video"]},
			{"id":"gpt-image-1","name":"GPT Image","maxInputTokens":8192,"maxOutputTokens":8192,"tags":["image-to-image"]},
			{"id":"glm-5.2","name":"GLM","maxInputTokens":131072,"maxOutputTokens":8192,"tags":["chat"]}
		],"agents":[{"name":"cli","models":["seedance-2.5","gpt-image-1","glm-5.2"]}]}}`), nil
	})
	infos, err := c.FetchModels(authStub())
	if err != nil {
		t.Fatalf("fetch models: %v", err)
	}
	if len(infos) != 1 || infos[0].ID != "glm-5.2" {
		t.Fatalf("expected only glm-5.2 (生成类模型应被过滤), got %+v", infos)
	}
}

// TestFetchModelsKeepsNormalChatModels P2：正常对话模型（deepseek/glm/kimi）不被过滤。
func TestFetchModelsKeepsNormalChatModels(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"data":{"models":[
			{"id":"deepseek-v4-flash","name":"DS","maxInputTokens":131072,"maxOutputTokens":8192},
			{"id":"glm-5.2","name":"GLM","maxInputTokens":131072,"maxOutputTokens":8192},
			{"id":"kimi-k2.7","name":"Kimi","maxInputTokens":131072,"maxOutputTokens":8192}
		],"agents":[{"name":"cli","models":["deepseek-v4-flash","glm-5.2","kimi-k2.7"]}]}}`), nil
	})
	infos, err := c.FetchModels(authStub())
	if err != nil {
		t.Fatalf("fetch models: %v", err)
	}
	if len(infos) != 3 {
		t.Fatalf("expected 3 normal chat models, got %d: %+v", len(infos), infos)
	}
}
