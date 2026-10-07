package llm

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	agentcore "deeptalk/internal/agent/core"
	"deeptalk/internal/agent/guard"
	agenttool "deeptalk/internal/agent/tool"
	"deeptalk/internal/infra/config"
	"deeptalk/internal/infra/metrics"

	"github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// fallbackModels 按配置构造备用模型。
//
// 备用模型与主模型共用同一 baseURL / apiKey，只换模型名——这是"同一家上游
// 换个更稳的型号"这一最常见兜底场景。构造失败的型号跳过并记日志，
// 不让一个写错的型号名把整个 Agent 组装搞挂。
func fallbackModels(ctx context.Context, conf *config.Config, baseURL, apiKey, primary string) []model.ToolCallingChatModel {
	var out []model.ToolCallingChatModel
	for _, name := range conf.LLMConfig.FallbackModels {
		if name == "" || name == primary {
			continue
		}
		m, err := openai.NewChatModel(ctx, &openai.ChatModelConfig{
			BaseURL: baseURL,
			Model:   name,
			APIKey:  apiKey,
		})
		if err != nil {
			log.Printf("[agent] fallback model %s unavailable: %v", name, err)
			continue
		}
		out = append(out, m)
	}
	return out
}

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

	ac := conf.GetAgent()
	// 把 [agent] 里的工具策略下沉给工具层，避免每个工具重复配置
	agenttool.Configure(time.Duration(ac.ToolTimeoutSeconds)*time.Second, ac.ToolMaxAttempts)

	// 兜底链：主模型失败 → 原模型重试 → 依次尝试备用模型 → 错误码
	chain := guard.NewFallbackModel(chat, fallbackModels(ctx, conf, baseURL, key, modelName), conf.LLMConfig.FallbackModels)

	maxStep := ac.MaxSteps
	if maxStep <= 0 {
		maxStep = 5
	}

	agent, err := agentcore.NewReActAgent(ctx, agentcore.ReActConfig{
		Model:   chain,
		Tools:   tools,
		MaxStep: maxStep,
		Limits: guard.Limits{
			MaxSteps:     ac.MaxSteps,
			MaxTokens:    ac.MaxTokens,
			MaxWallClock: time.Duration(ac.MaxWallClockSeconds) * time.Second,
			ModelTimeout: time.Duration(ac.ModelTimeoutSeconds) * time.Second,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("create unified agent failed: %v", err)
	}

	if len(tools) == 0 {
		// 没有工具时确实退化成了普通对话：这是配置/上游的问题，
		// 用指标暴露出来，否则"Agent 其实没在调工具"完全看不出来
		metrics.CountAgentDegraded("plain_chat")
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
