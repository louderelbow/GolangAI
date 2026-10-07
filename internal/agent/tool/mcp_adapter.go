package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"deeptalk/internal/infra/resilience"

	einotool "github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
)

// ======================== MCP 工具 → eino 工具 ========================

// mcpTool 把一个 MCP 工具包装成 eino 可调用工具
type mcpTool struct {
	name   string
	desc   string
	params *schema.ToolInfo
	server *mcpServer
	client *client.Client
	origin string // 服务端原始工具名（可能带前缀）
}

func newMCPTool(name string, t mcp.Tool, server *mcpServer, c *client.Client) *mcpTool {
	return &mcpTool{
		name:   name,
		desc:   t.Description,
		params: ToolInfoFromMCP(name, t),
		server: server,
		client: c,
		origin: t.Name,
	}
}

// Info eino 工具元信息（模型据此决定怎么调用）
func (t *mcpTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return t.params, nil
}

// InvokableRun 执行工具：argumentsInJSON 由模型按 ToolInfo 生成
func (t *mcpTool) InvokableRun(ctx context.Context, argumentsInJSON string, opts ...einotool.Option) (string, error) {
	args := map[string]interface{}{}
	if strings.TrimSpace(argumentsInJSON) != "" {
		if err := json.Unmarshal([]byte(argumentsInJSON), &args); err != nil {
			return "", fmt.Errorf("invalid tool arguments: %w", err)
		}
	}

	// 工具调用必须留痕：否则"模型没调工具、自己编答案"这种情况完全看不出来
	start := time.Now()
	log.Printf("[MCP] CALL tool=%s args=%s", t.name, truncate(argumentsInJSON, 200))

	// 单次调用超时：一个卡住的工具不能拖死整个 Agent 循环
	callCtx, cancel := context.WithTimeout(ctx, defaultToolTimeout)
	defer cancel()

	// 每个工具一个熔断器：某个工具服务挂了不会拖垮其它工具
	res, err := resilience.Do(resilience.MCPKey(t.name), func() (*mcp.CallToolResult, error) {
		return t.client.CallTool(callCtx, mcp.CallToolRequest{
			Params: mcp.CallToolParams{Name: t.origin, Arguments: args},
		})
	})
	if err != nil {
		log.Printf("[MCP] tool=%s failed after %s: %v", t.name, time.Since(start).Round(time.Millisecond), err)
		return "", fmt.Errorf("mcp call %s failed: %w", t.origin, err)
	}

	var sb strings.Builder
	for _, content := range res.Content {
		if text, ok := content.(mcp.TextContent); ok {
			sb.WriteString(text.Text)
			sb.WriteString("\n")
		}
	}
	out := strings.TrimSpace(sb.String())
	log.Printf("[MCP] tool=%s done in %s, result=%d chars", t.name, time.Since(start).Round(time.Millisecond), len(out))

	if res.IsError {
		return out, fmt.Errorf("tool %s returned error: %s", t.origin, out)
	}
	return out, nil
}

// truncate 日志截断，避免把整段参数写进日志
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// toolInfoFromMCP 把 MCP 的 JSON Schema 参数转换成 eino 的 ToolInfo
func ToolInfoFromMCP(name string, t mcp.Tool) *schema.ToolInfo {
	params := make(map[string]*schema.ParameterInfo, len(t.InputSchema.Properties))
	for propName, raw := range t.InputSchema.Properties {
		prop, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		typ := schema.String
		if s, ok := prop["type"].(string); ok {
			switch s {
			case "integer":
				typ = schema.Integer
			case "number":
				typ = schema.Number
			case "boolean":
				typ = schema.Boolean
			case "array":
				typ = schema.Array
			case "object":
				typ = schema.Object
			}
		}
		desc, _ := prop["description"].(string)
		required := false
		for _, r := range t.InputSchema.Required {
			if r == propName {
				required = true
			}
		}
		params[propName] = &schema.ParameterInfo{Type: typ, Desc: desc, Required: required}
	}

	info := &schema.ToolInfo{Name: name, Desc: t.Description}
	if len(params) > 0 {
		info.ParamsOneOf = schema.NewParamsOneOfByParams(params)
	}
	return info
}
