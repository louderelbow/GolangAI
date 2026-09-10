package aihelper

import (
	"math"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

// 构造一个和真实 MCP 天气工具同构的 Tool
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

func TestCosine(t *testing.T) {
	a := []float64{1, 0, 0}
	b := []float64{1, 0, 0}
	if got := cosine(a, b); math.Abs(got-1) > 1e-9 {
		t.Errorf("相同向量相似度应为 1，实际 %v", got)
	}

	c := []float64{0, 1, 0}
	if got := cosine(a, c); math.Abs(got) > 1e-9 {
		t.Errorf("正交向量相似度应为 0，实际 %v", got)
	}

	d := []float64{-1, 0, 0}
	if got := cosine(a, d); math.Abs(got+1) > 1e-9 {
		t.Errorf("相反向量相似度应为 -1，实际 %v", got)
	}

	// 维度不一致 / 空向量不能 panic
	if got := cosine(a, []float64{1, 2}); got != -1 {
		t.Errorf("维度不一致应返回 -1，实际 %v", got)
	}
	if got := cosine([]float64{}, []float64{}); got != -1 {
		t.Errorf("空向量应返回 -1，实际 %v", got)
	}
}

// MCP 的 JSON Schema 要能转成 eino 的 ToolInfo（原生 function calling 的基础）
func TestToolInfoFromMCP(t *testing.T) {
	tool := mcpToolForTest()
	info := toolInfoFromMCP("weather__get_weather", tool)

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
