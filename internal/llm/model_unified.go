package llm

import (
	"context"
	"fmt"
	"log"
	"os"

	agentcore "deeptalk/internal/agent/core"
	agenttool "deeptalk/internal/agent/tool"
	"deeptalk/internal/infra/config"

	"github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/schema"
)

// =================== Unified Agent 实现（原生 function calling） ===================
//
// 工具来源与白名单全部走配置（[mcpConfig.servers]）：
// 启动时从各 MCP 服务端拉取 tools/list，包装成 eino 工具，交给 Agent 让模型
// 用**协议原生**的 tool_calls 调用 —— 不依赖"让模型吐 JSON 再解析"。
//
// Agent 循环本身在 internal/agent/core，本文件只负责：
//   1) 构造底层模型（协议客户端）
//   2) 汇总工具（本地注册 + MCP 来源）
//   3) 组装 Agent，并把它适配成 AIModel 接口
// 这样 guard / trace 等能力将来只需装饰 core.Agent，不必改动这里。

// UnifiedModel 统一 Agent：Agent 循环 + 工具（MCP / 本地）+ 原生 function calling。
type UnifiedModel struct {
	agent agentcore.Agent
	name  string
}

// NewUnifiedModel 创建 Unified Agent 实例。
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

	chat, err := openai.NewChatModel(ctx, &openai.ChatModelConfig{
		BaseURL: baseURL,
		Model:   modelName,
		APIKey:  key,
	})
	if err != nil {
		return nil, fmt.Errorf("create unified model failed: %v", err)
	}

	// 工具来源：本地注册（PHASE-8 的 skill 会向这里注册）+ MCP 注册表
	registry := agenttool.NewRegistry()
	registry.AddSource(agenttool.GetMCPRegistry().AsSource())
	tools := registry.Tools(ctx)

	maxStep := conf.McpConfig.MaxStep
	if maxStep <= 0 {
		maxStep = 5
	}

	agent, err := agentcore.NewReActAgent(ctx, agentcore.ReActConfig{
		Model:   chat,
		Tools:   tools,
		MaxStep: maxStep,
	})
	if err != nil {
		return nil, fmt.Errorf("create unified agent failed: %v", err)
	}

	if len(tools) == 0 {
		// 没有任何工具时退化为普通对话，而不是直接报错
		log.Printf("[MCP] no tools available, fallback to plain chat (user=%s)", username)
	}
	log.Printf("[MCP] agent ready: user=%s tools=%d maxStep=%d", username, len(tools), maxStep)

	return &UnifiedModel{agent: agent, name: modelName}, nil
}

// GenerateResponse 生成响应（有工具时走 Agent 循环）。
func (m *UnifiedModel) GenerateResponse(ctx context.Context, messages []*schema.Message) (*schema.Message, error) {
	resp, err := m.agent.Run(ctx, agentcore.Request{Messages: messages})
	if err != nil {
		return nil, err
	}
	return assistantMessage(resp), nil
}

// StreamResponse 流式响应（有工具时走 Agent 循环）。
func (m *UnifiedModel) StreamResponse(ctx context.Context, messages []*schema.Message, cb StreamCallback) (string, *schema.TokenUsage, error) {
	resp, err := m.agent.Stream(ctx, agentcore.Request{Messages: messages}, agentcore.StreamCallback(cb))
	if err != nil {
		// 流式中断时已产出的内容仍然返回：用户应看到已有的回答
		if resp != nil {
			return resp.Content, resp.Usage, err
		}
		return "", nil, err
	}
	return resp.Content, resp.Usage, nil
}

// assistantMessage 把 Agent 输出还原成 eino 消息，供上层统一处理。
func assistantMessage(resp *agentcore.Response) *schema.Message {
	msg := &schema.Message{Role: schema.Assistant}
	if resp != nil {
		msg.Content = resp.Content
		if resp.Usage != nil {
			msg.ResponseMeta = &schema.ResponseMeta{Usage: resp.Usage}
		}
	}
	return msg
}

// GetModelType 获取模型类型
func (m *UnifiedModel) GetModelType() string { return ModelTypeUnified }

// GetModelName 获取模型名
func (m *UnifiedModel) GetModelName() string { return m.name }
