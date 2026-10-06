package tool

import (
	"context"
	"fmt"
	"sync"

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
