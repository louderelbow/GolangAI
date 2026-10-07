package tool

import (
	"context"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"deeptalk/internal/infra/config"

	"github.com/cloudwego/eino/components/tool"
	"github.com/mark3labs/mcp-go/mcp"
)

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

// ======================== 接入通用工具注册表 ========================

// AsSource 把 MCP 注册表适配成通用的工具来源。
//
// 这样 Agent 只认识 tool.Registry，不关心工具来自本地代码还是 MCP 服务端；
// 将来接入新的工具来源只需实现 Source 接口。
func (r *MCPRegistry) AsSource() Source { return mcpSource{registry: r} }

type mcpSource struct {
	registry *MCPRegistry
}

func (s mcpSource) Name() string { return "mcp" }

func (s mcpSource) Tools(ctx context.Context) []tool.BaseTool {
	return s.registry.Tools(ctx)
}
