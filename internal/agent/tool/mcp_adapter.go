package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"deeptalk/internal/agent/trace"
	"deeptalk/internal/infra/metrics"
	"deeptalk/internal/infra/resilience"

	einotool "github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/sony/gobreaker/v2"
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
	// 与本地工具同一条轨迹入口：MCP 工具也是工具，
	// 分成两套记录会让"工具成功率"只能看到一半。
	sp := trace.Begin(ctx, trace.KindTool, t.name)

	// 与本地工具同一道循环检测（见 spec.go 与 loopguard.go）。
	if g := loopGuardFrom(ctx); g != nil {
		if n, allFailed, blocked := g.Blocked(t.name, argumentsInJSON); blocked {
			sp.End(trace.StatusRejected, "重复调用，已打断")
			metrics.CountAgentLoopDetected(t.name)
			notifyLoop(ctx, t.name, n, allFailed)
			return loopObservation(t.name, argumentsInJSON, n, allFailed), nil
		}
	}

	args := map[string]interface{}{}
	if strings.TrimSpace(argumentsInJSON) != "" {
		if err := json.Unmarshal([]byte(argumentsInJSON), &args); err != nil {
			// 参数坏掉是模型的问题，不该让整轮失败——回填一句可操作的话，
			// 让它自己决定改参数重试还是直接作答。
			sp.End(trace.StatusError, "参数不是合法 JSON")
			return toolFailureObservation(ctx, t.name, fmt.Errorf("参数不是合法 JSON: %w", err)), nil
		}
	}

	// 工具调用必须留痕：否则"模型没调工具、自己编答案"这种情况完全看不出来
	start := time.Now()
	log.Printf("[MCP] CALL tool=%s args=%s", t.name, truncate(argumentsInJSON, 200))

	// 单次调用超时：一个卡住的工具不能拖死整个 Agent 循环
	callCtx, cancel := context.WithTimeout(ctx, defaultToolTimeout)
	defer cancel()

	// 每个工具一个熔断器：某个工具服务挂了不会拖垮其它工具。
	//
	// "工具执行成功但结果里带业务错误"（IsError）也放在闭包**内部**判断：
	// 放到外面的话 resilience.Do 看到的是"调用成功"，这种持续报错的工具
	// 永远不会被熔断，每次都要白等一轮超时。
	res, err := resilience.Do(resilience.ToolKey(t.name), func() (*mcp.CallToolResult, error) {
		res, err := t.client.CallTool(callCtx, mcp.CallToolRequest{
			Params: mcp.CallToolParams{Name: t.origin, Arguments: args},
		})
		if err != nil {
			return nil, err
		}
		if res.IsError {
			return nil, fmt.Errorf("工具返回错误: %s", textOf(res))
		}
		return res, nil
	})
	if err != nil {
		log.Printf("[MCP] tool=%s failed after %s: %v", t.name, time.Since(start).Round(time.Millisecond), err)
		// 熔断打开是策略拒绝，不是工具故障：把它分开，否则"熔断在保护你"
		// 和"工具真的坏了"在指标上长得一模一样。
		if errors.Is(err, gobreaker.ErrOpenState) || errors.Is(err, gobreaker.ErrTooManyRequests) {
			sp.End(trace.StatusRejected, "熔断器已打开")
		} else {
			sp.EndErr(err, "")
		}
		// 回填而不是上抛：整轮不该因为一个工具抖动而丢掉回答
		loopGuardFrom(ctx).Failed(t.name, argumentsInJSON)
		return toolFailureObservation(ctx, t.name, err), nil
	}

	out := strings.TrimSpace(textOf(res))
	log.Printf("[MCP] tool=%s done in %s, result=%d chars", t.name, time.Since(start).Round(time.Millisecond), len(out))
	loopGuardFrom(ctx).Succeeded(t.name, argumentsInJSON)
	sp.EndOK(out)
	return out, nil
}

// textOf 把 MCP 结果里的文本内容拼起来；非文本（图片、资源）当前丢弃。
func textOf(res *mcp.CallToolResult) string {
	if res == nil {
		return ""
	}
	var sb strings.Builder
	for _, content := range res.Content {
		if text, ok := content.(mcp.TextContent); ok {
			sb.WriteString(text.Text)
			sb.WriteString("\n")
		}
	}
	return sb.String()
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
