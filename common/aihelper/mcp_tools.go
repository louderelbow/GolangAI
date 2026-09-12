package aihelper

import (
	"context"
	"deeptalk/common/resilience"
	"deeptalk/config"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
)

type mcpServer struct {
	name         string
	url          string
	command      string
	args         []string
	env          []string
	allowedTools []string

	mu     sync.Mutex
	client *client.Client
	tools  []mcp.Tool
}

func (s *mcpServer) ensureClient(ctx context.Context) (*client.Client, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.client != nil {
		return s.client, nil
	}

	var c *client.Client
	var err error

	switch {
	case s.url != "":
		// 远程服务：StreamableHTTP
		var httpTransport *transport.StreamableHTTP
		httpTransport, err = transport.NewStreamableHTTP(s.url)
		if err != nil {
			return nil, fmt.Errorf("create transport for %s failed: %w", s.name, err)
		}
		c = client.NewClient(httpTransport)

	case s.command != "":
		// 本地子进程：stdio（社区工具大多是 npx/uvx 启动的）
		// Windows 上 npx/uvx 实际是可执行脚本（npx.cmd / uvx.exe），
		// 直接 exec "npx" 会报 "不是有效的 Win32 应用程序"，这里做一次兜底解析
		cmd := resolveCommand(s.command)
		c, err = client.NewStdioMCPClient(cmd, s.env, s.args...)
		if err != nil {
			return nil, fmt.Errorf("start stdio server %s (%s) failed: %w", s.name, cmd, err)
		}

	default:
		return nil, fmt.Errorf("mcp server %s: neither url nor command configured", s.name)
	}

	initReq := mcp.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcp.Implementation{Name: "DeepTalk", Version: "1.0.0"}
	initReq.Params.Capabilities = mcp.ClientCapabilities{}

	if _, err := c.Initialize(ctx, initReq); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("initialize %s failed: %w", s.name, err)
	}

	s.client = c
	return s.client, nil
}

// allowed 工具是否在白名单内（白名单为空表示全部允许）
func (s *mcpServer) allowed(toolName string) bool {
	if len(s.allowedTools) == 0 {
		return true
	}
	for _, t := range s.allowedTools {
		if t == toolName {
			return true
		}
	}
	return false
}

// MCPRegistry MCP 工具注册表
type MCPRegistry struct {
	servers []*mcpServer

	mu       sync.RWMutex
	tools    []tool.BaseTool
	loaded   bool
	loadedAt time.Time
	err      error
}

// emptyRetryInterval 一次都没拉到工具时的重试间隔：
// 否则"启动时 MCP 服务没起来"会被永久缓存，之后必须重启后端才能用上工具
const emptyRetryInterval = 60 * time.Second
const mcpServerTimeout = 15 * time.Second

var (
	globalMCPRegistry *MCPRegistry
	mcpOnce           sync.Once
)

// GetMCPRegistry 获取全局注册表（从配置构造）
func GetMCPRegistry() *MCPRegistry {
	mcpOnce.Do(func() {
		cfg := config.GetConfig()

		servers := make([]*mcpServer, 0, len(cfg.McpConfig.Servers))
		for _, s := range cfg.McpConfig.Servers {
			if s.URL == "" && s.Command == "" {
				continue
			}
			servers = append(servers, &mcpServer{
				name:         firstNonEmpty(s.Name, s.Command, s.URL),
				url:          s.URL,
				command:      s.Command,
				args:         s.Args,
				env:          s.Env,
				allowedTools: s.AllowedTools,
			})
		}

		// 向后兼容：没有配置时沿用环境变量 / 默认的本地 MCP 服务
		if len(servers) == 0 {
			url := os.Getenv("MCP_BASE_URL")
			if url == "" {
				url = "http://localhost:8081/mcp"
			}
			servers = append(servers, &mcpServer{name: "default", url: url})
		}

		globalMCPRegistry = &MCPRegistry{servers: servers}
	})
	return globalMCPRegistry
}

// resolveCommand 解析 stdio 启动命令
func resolveCommand(cmd string) string {
	if runtime.GOOS != "windows" || filepath.Ext(cmd) != "" {
		return cmd
	}
	for _, ext := range []string{".cmd", ".bat", ".exe"} {
		if p, err := exec.LookPath(cmd + ext); err == nil {
			return p
		}
	}
	return cmd
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// Tools 返回已加载的 eino 工具（首次调用会触发加载，失败时返回空列表并记录错误）
func (r *MCPRegistry) Tools(ctx context.Context) []tool.BaseTool {
	r.mu.RLock()
	if r.loaded {
		tools := r.tools
		stale := len(tools) == 0 && time.Since(r.loadedAt) > emptyRetryInterval
		r.mu.RUnlock()
		if !stale {
			return tools
		}
		// 上次一个工具都没拉到：过一会儿再试，避免必须重启服务
		log.Printf("[MCP] no tools cached, retrying registry load")
	} else {
		r.mu.RUnlock()
	}

	r.load(ctx)

	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.tools
}

// Reload 强制重新拉取工具清单（服务端新增工具后调用）
func (r *MCPRegistry) Reload(ctx context.Context) {
	r.mu.Lock()
	r.loaded = false
	r.mu.Unlock()
	r.load(ctx)
}

func (r *MCPRegistry) load(ctx context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.loaded {
		return
	}

	tools := make([]tool.BaseTool, 0)
	var lastErr error

	for _, srv := range r.servers {
		loadCtx, cancel := context.WithTimeout(ctx, mcpServerTimeout)
		c, err := srv.ensureClient(loadCtx)
		if err != nil {
			cancel()
			lastErr = err
			log.Printf("[MCP] server %s unavailable: %v", srv.name, err)
			continue
		}

		res, err := c.ListTools(loadCtx, mcp.ListToolsRequest{})
		cancel()
		if err != nil {
			lastErr = err
			log.Printf("[MCP] list tools from %s failed: %v", srv.name, err)
			continue
		}

		kept := make([]string, 0, len(res.Tools))
		for _, t := range res.Tools {
			if !srv.allowed(t.Name) {
				log.Printf("[MCP] tool %s from %s skipped (not in allowlist)", t.Name, srv.name)
				continue
			}
			name := t.Name
			// 多服务端时给工具名加前缀，避免重名覆盖
			if len(r.servers) > 1 {
				name = srv.name + "__" + t.Name
			}
			tools = append(tools, newMCPTool(name, t, srv, c))
			kept = append(kept, name)
		}
		log.Printf("[MCP] server %s loaded %d tools: %v", srv.name, len(kept), kept)
	}

	if len(tools) == 0 && lastErr != nil {
		r.err = lastErr
	}

	r.tools = tools
	r.loaded = true
	r.loadedAt = time.Now()
	log.Printf("[MCP] registry ready: %d tools from %d servers", len(tools), len(r.servers))
}

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
		params: toolInfoFromMCP(name, t),
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
func (t *mcpTool) InvokableRun(ctx context.Context, argumentsInJSON string, opts ...tool.Option) (string, error) {
	args := map[string]interface{}{}
	if strings.TrimSpace(argumentsInJSON) != "" {
		if err := json.Unmarshal([]byte(argumentsInJSON), &args); err != nil {
			return "", fmt.Errorf("invalid tool arguments: %w", err)
		}
	}

	// 工具调用必须留痕：否则"模型没调工具、自己编答案"这种情况完全看不出来
	start := time.Now()
	log.Printf("[MCP] CALL tool=%s args=%s", t.name, truncate(argumentsInJSON, 200))

	// 每个工具一个熔断器：某个工具服务挂了不会拖垮其它工具
	res, err := resilience.Do(resilience.MCPKey(t.name), func() (*mcp.CallToolResult, error) {
		return t.client.CallTool(ctx, mcp.CallToolRequest{
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
func toolInfoFromMCP(name string, t mcp.Tool) *schema.ToolInfo {
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
