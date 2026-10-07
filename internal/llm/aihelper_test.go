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
	for _, modelType := range []string{ModelTypeRAG, ModelTypeUnified} {
		if !IsValidModelType(modelType) {
			t.Errorf("model type %q should be valid", modelType)
		}
	}
	for _, modelType := range []string{"", "0", "1", "3", "4", "5", "abc"} {
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

// ==================== 模型类型 ====================

// TestNormalizeModelTypeDropsDeepSeek 原 modelType 1（DeepSeek 纯对话）已删除。
//
// 历史会话必须仍能打开，所以它要能映射到一条还活着的路径；
// 映射目标是 6 而不是 2：它原本就不带检索，映射到 RAG 会让一个
// 从来不查文档的会话突然开始按文档回答，答案性质变了。
func TestNormalizeModelTypeDropsDeepSeek(t *testing.T) {
	got, ok := NormalizeModelType("1")
	if !ok {
		t.Fatal("历史 modelType 1 的会话必须仍可打开")
	}
	if got != ModelTypeUnified {
		t.Errorf("应映射到 Unified Agent(6)，实际 %s", got)
	}

	if !IsValidModelType(ModelTypeRAG) || !IsValidModelType(ModelTypeUnified) {
		t.Error("2 / 6 应仍然有效")
	}
	if IsValidModelType("1") {
		t.Error("已删除的 modelType 1 不该还能创建新会话")
	}
}
