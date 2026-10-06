package llm

import (
	"context"
	agenttool "deeptalk/internal/agent/tool"
	"deeptalk/internal/infra/config"
	"fmt"
	"io"
	"log"
	"os"
	"strings"

	"github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/flow/agent/react"
	"github.com/cloudwego/eino/schema"
)

// =================== Unified Agent 实现（原生 function calling） ===================
//
// 工具来源与白名单全部走配置（[mcpConfig.servers]），见 mcp_tools.go：
// 启动时从各 MCP 服务端拉取 tools/list，包装成 eino 工具，交给 Agent 让模型
// 用**协议原生**的 tool_calls 调用 —— 不再依赖"让模型吐 JSON 再解析"。

type UnifiedModel struct {
	llm   model.ToolCallingChatModel
	agent *react.Agent
	name  string
}

// NewUnifiedModel 创建Unified Agent实例
func NewUnifiedModel(ctx context.Context, username string) (*UnifiedModel, error) {
	conf := config.GetConfig()
	key := conf.RagModelConfig.RagApiKey
	if key == "" {
		key = os.Getenv("ALIYUN_API_KEY")
	}
	if key == "" {
		key = os.Getenv("DEEPSEEK_API_KEY")
	}
	if key == "" {
		key = os.Getenv("OPENAI_API_KEY")
	}
	modelName := conf.RagModelConfig.RagChatModelName
	baseURL := conf.RagModelConfig.RagBaseUrl

	llm, err := openai.NewChatModel(ctx, &openai.ChatModelConfig{
		BaseURL: baseURL,
		Model:   modelName,
		APIKey:  key,
	})
	if err != nil {
		return nil, fmt.Errorf("create unified model failed: %v", err)
	}

	m := &UnifiedModel{llm: llm, name: modelName}

	// 拉取 MCP 工具（注册表内部带缓存与白名单过滤）
	tools := agenttool.GetMCPRegistry().Tools(ctx)
	if len(tools) == 0 {
		// 没有任何工具时退化为普通对话，而不是直接报错
		log.Printf("[MCP] no tools available, fallback to plain chat (user=%s)", username)
		return m, nil
	}

	maxStep := conf.McpConfig.MaxStep
	if maxStep <= 0 {
		maxStep = 5
	}

	agent, err := react.NewAgent(ctx, &react.AgentConfig{
		ToolCallingModel: llm,
		ToolsConfig: compose.ToolsNodeConfig{
			Tools: tools,
		},
		MaxStep: maxStep,
	})
	if err != nil {
		return nil, fmt.Errorf("create unified agent failed: %v", err)
	}
	m.agent = agent
	log.Printf("[MCP] agent ready: user=%s tools=%d maxStep=%d", username, len(tools), maxStep)
	return m, nil
}

// GenerateResponse 生成响应（有工具时走 Agent，原生 function calling）
func (m *UnifiedModel) GenerateResponse(ctx context.Context, messages []*schema.Message) (*schema.Message, error) {
	if len(messages) == 0 {
		return nil, fmt.Errorf("no messages provided")
	}
	if m.agent == nil {
		return m.llm.Generate(ctx, messages)
	}

	resp, err := m.agent.Generate(ctx, messages)
	if err != nil {
		return nil, fmt.Errorf("unified agent generate failed: %v", err)
	}
	return resp, nil
}

// StreamResponse 流式响应（有工具时走 Agent，原生 function calling）
func (m *UnifiedModel) StreamResponse(ctx context.Context, messages []*schema.Message, cb StreamCallback) (string, *schema.TokenUsage, error) {
	if len(messages) == 0 {
		return "", nil, fmt.Errorf("no messages provided")
	}

	if m.agent == nil {
		stream, err := m.llm.Stream(ctx, messages)
		if err != nil {
			return "", nil, fmt.Errorf("mcp stream failed: %v", err)
		}
		defer stream.Close()
		var sb strings.Builder
		var usage *schema.TokenUsage
		for {
			msg, err := stream.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				return sb.String(), usage, err
			}
			usage = pickUsage(usage, msg)
			if len(msg.Content) > 0 {
				sb.WriteString(msg.Content)
				cb(msg.Content)
			}
		}
		return sb.String(), usage, nil
	}

	stream, err := m.agent.Stream(ctx, messages)
	if err != nil {
		return "", nil, fmt.Errorf("unified agent stream failed: %v", err)
	}
	defer stream.Close()

	var finalResp strings.Builder
	var usage *schema.TokenUsage

	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return finalResp.String(), usage, fmt.Errorf("unified agent stream recv failed: %v", err)
		}
		usage = pickUsage(usage, msg)
		if len(msg.Content) > 0 {
			finalResp.WriteString(msg.Content)
			cb(msg.Content)
		}
	}

	return finalResp.String(), usage, nil
}

// GetModelType 获取模型类型
func (m *UnifiedModel) GetModelType() string { return ModelTypeUnified }

// GetModelName 获取模型名
func (m *UnifiedModel) GetModelName() string { return m.name }
