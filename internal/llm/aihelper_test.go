package llm

import (
	"math"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	agenttool "deeptalk/internal/agent/tool"
	cachepkg "deeptalk/internal/cache"

	"github.com/cloudwego/eino/schema"
	"github.com/mark3labs/mcp-go/mcp"
)

func TestMain(m *testing.M) {
	if _, file, _, ok := runtime.Caller(0); ok {
		root := filepath.Dir(filepath.Dir(filepath.Dir(file)))
		_ = os.Setenv("DEEPTALK_CONFIG", filepath.Join(root, "config", "config.toml.example"))
	}
	os.Exit(m.Run())
}

func TestIsValidModelType(t *testing.T) {
	for _, modelType := range []string{ModelTypeDeepSeek, ModelTypeRAG, ModelTypeUnified} {
		if !IsValidModelType(modelType) {
			t.Errorf("model type %q should be valid", modelType)
		}
	}
	for _, modelType := range []string{"", "0", "3", "4", "5", "abc"} {
		if IsValidModelType(modelType) {
			t.Errorf("model type %q should be invalid", modelType)
		}
	}
}

// ==================== RAG 相关性过滤 ====================

// RAG 相关性过滤必须兼容 Redis 返回的字符串 distance
func TestFilterByRelevanceWithStringDistance(t *testing.T) {
	rag := &AliRAGModel{}
	docs := []*schema.Document{
		{ID: "a", Content: "最相关", MetaData: map[string]any{"distance": "0.10"}},
		{ID: "b", Content: "一般相关", MetaData: map[string]any{"distance": "0.35"}},
		{ID: "c", Content: "不相关", MetaData: map[string]any{"distance": "0.90"}},
		{ID: "d", Content: "没有距离字段", MetaData: map[string]any{}},
	}

	got := rag.filterByRelevance(docs)
	if len(got) != 3 {
		ids := make([]string, 0, len(got))
		for _, d := range got {
			ids = append(ids, d.ID)
		}
		t.Fatalf("string distance not handled, kept=%v, want [a b d]", ids)
	}

	// float64（部分实现会预先解析）同样要支持
	docsFloat := []*schema.Document{
		{ID: "a", MetaData: map[string]any{"distance": 0.1}},
		{ID: "c", MetaData: map[string]any{"distance": 0.9}},
	}
	if got := rag.filterByRelevance(docsFloat); len(got) != 1 || got[0].ID != "a" {
		t.Fatalf("float distance filter wrong: %+v", got)
	}
}

// ==================== 语义缓存 ====================

func TestCosine(t *testing.T) {
	a := []float64{1, 0, 0}
	b := []float64{1, 0, 0}
	if got := cachepkg.Cosine(a, b); math.Abs(got-1) > 1e-9 {
		t.Errorf("相同向量相似度应为 1，实际 %v", got)
	}

	c := []float64{0, 1, 0}
	if got := cachepkg.Cosine(a, c); math.Abs(got) > 1e-9 {
		t.Errorf("正交向量相似度应为 0，实际 %v", got)
	}

	d := []float64{-1, 0, 0}
	if got := cachepkg.Cosine(a, d); math.Abs(got+1) > 1e-9 {
		t.Errorf("相反向量相似度应为 -1，实际 %v", got)
	}

	// 维度不一致 / 空向量不能 panic
	if got := cachepkg.Cosine(a, []float64{1, 2}); got != -1 {
		t.Errorf("维度不一致应返回 -1，实际 %v", got)
	}
	if got := cachepkg.Cosine([]float64{}, []float64{}); got != -1 {
		t.Errorf("空向量应返回 -1，实际 %v", got)
	}
}

// ==================== MCP 工具（原生 function calling） ====================

// mcpToolForTest 构造一个和真实 MCP 天气工具同构的 Tool
func mcpToolForTest() mcp.Tool {
	return mcp.Tool{
		Name:        "get_weather",
		Description: "获取指定城市的天气信息",
		InputSchema: mcp.ToolInputSchema{
			Type: "object",
			Properties: map[string]interface{}{
				"city": map[string]interface{}{
					"type":        "string",
					"description": "城市名称，如 Beijing、上海",
				},
			},
			Required: []string{"city"},
		},
	}
}

// MCP 的 JSON Schema 要能转成 eino 的 ToolInfo（原生 function calling 的基础）
func TestToolInfoFromMCP(t *testing.T) {
	tool := mcpToolForTest()
	info := agenttool.ToolInfoFromMCP("weather__get_weather", tool)

	if info.Name != "weather__get_weather" {
		t.Errorf("name = %s", info.Name)
	}
	if info.Desc == "" {
		t.Error("desc 不应为空")
	}
	params := info.ParamsOneOf
	if params == nil {
		t.Fatal("ParamsOneOf 不应为空")
	}
	got, err := params.ToJSONSchema()
	if err != nil {
		t.Fatalf("ToJSONSchema: %v", err)
	}
	if got == nil || got.Properties == nil || got.Properties.Len() == 0 {
		t.Fatalf("参数 schema 未生成: %+v", got)
	}
	if _, ok := got.Properties.Get("city"); !ok {
		t.Errorf("缺少 city 参数")
	}
	if len(got.Required) != 1 || got.Required[0] != "city" {
		t.Errorf("required 解析异常: %v", got.Required)
	}
}

// ==================== 模型连接配置解析 ====================

// deepSeekSettings：环境变量优先，缺失时回落 DEEPSEEK_* → OPENAI_* → 默认值
func TestDeepSeekSettingsFromEnv(t *testing.T) {
	// 用例 1：DEEPSEEK_* 存在时优先采用
	t.Setenv("DEEPSEEK_API_KEY", "env-deepseek-key")
	t.Setenv("DEEPSEEK_MODEL_NAME", "env-model")
	t.Setenv("DEEPSEEK_BASE_URL", "http://env.example.com")
	t.Setenv("OPENAI_API_KEY", "env-openai-key")

	baseURL, modelName, key := deepSeekSettings()
	if key != "env-deepseek-key" || modelName != "env-model" || baseURL != "http://env.example.com" {
		t.Errorf("DEEPSEEK_* 环境变量未生效: base=%s model=%s key=%s", baseURL, modelName, key)
	}

	// 用例 2：缺失时回落 OPENAI_* 与默认值
	os.Unsetenv("DEEPSEEK_API_KEY")
	os.Unsetenv("DEEPSEEK_MODEL_NAME")
	os.Unsetenv("DEEPSEEK_BASE_URL")

	baseURL, modelName, key = deepSeekSettings()
	if key != "env-openai-key" {
		t.Errorf("应回落 OPENAI_API_KEY，实际 %s", key)
	}
	if modelName != "deepseek-chat" {
		t.Errorf("模型名应回落默认 deepseek-chat，实际 %s", modelName)
	}
	if baseURL != "https://api.deepseek.com" {
		t.Errorf("baseURL 应回落默认 api.deepseek.com，实际 %s", baseURL)
	}
}
