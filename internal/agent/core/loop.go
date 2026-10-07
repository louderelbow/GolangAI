package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/cloudwego/eino/components/model"
	einotool "github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/flow/agent/react"
	"github.com/cloudwego/eino/schema"
)

// defaultMaxStep 是 ReAct 循环的步数上限。
//
// Agent 每一步都要一次模型调用（成本与延迟线性增长），而业务上的工具链
// 通常只有 1~3 步，5 是"够用且不会失控"的上限。真正严格的预算控制在 PHASE-2
// 由 guard 统一施加，这里只是兜底。
const defaultMaxStep = 5

// ReActConfig 构造 ReAct Agent 所需的依赖。
type ReActConfig struct {
	Model   model.ToolCallingChatModel
	Tools   []einotool.BaseTool
	MaxStep int
}

// reactAgent 承载"思考 → 调工具 → 观察 → 再思考"循环。
//
// 循环本身复用 eino 的 ReAct 实现；外面这层包壳的价值在于：
// 1) 给上层一个不依赖具体 Agent 框架的稳定接口；
// 2) 为 guard / trace 提供可装饰的接入点；
// 3) 工具为空时自动退化为普通对话，而不是让调用方特判。
type reactAgent struct {
	chat    model.ToolCallingChatModel
	inner   *react.Agent
	maxStep int
}

// NewReActAgent 构造 ReAct Agent。工具为空时退化为普通对话，不算错误。
func NewReActAgent(ctx context.Context, cfg ReActConfig) (Agent, error) {
	if cfg.Model == nil {
		return nil, errors.New("react agent: model is required")
	}

	maxStep := cfg.MaxStep
	if maxStep <= 0 {
		maxStep = defaultMaxStep
	}

	a := &reactAgent{chat: cfg.Model, maxStep: maxStep}
	if len(cfg.Tools) == 0 {
		return a, nil
	}

	inner, err := react.NewAgent(ctx, &react.AgentConfig{
		ToolCallingModel: cfg.Model,
		ToolsConfig:      compose.ToolsNodeConfig{Tools: cfg.Tools},
		MaxStep:          maxStep,
	})
	if err != nil {
		return nil, fmt.Errorf("build react agent: %w", err)
	}
	a.inner = inner
	return a, nil
}

// Run 一次性返回完整回答。
func (a *reactAgent) Run(ctx context.Context, req Request) (*Response, error) {
	if len(req.Messages) == 0 {
		return nil, errors.New("react agent: empty messages")
	}
	if a.inner == nil {
		msg, err := a.chat.Generate(ctx, req.Messages)
		if err != nil {
			return nil, fmt.Errorf("plain chat generate: %w", err)
		}
		return &Response{Content: msg.Content, Usage: usageOf(msg)}, nil
	}

	msg, err := a.inner.Generate(ctx, req.Messages)
	if err != nil {
		return nil, fmt.Errorf("react agent generate: %w", err)
	}
	return &Response{Content: msg.Content, Usage: usageOf(msg)}, nil
}

// Stream 边生成边回调。
func (a *reactAgent) Stream(ctx context.Context, req Request, cb StreamCallback) (*Response, error) {
	if len(req.Messages) == 0 {
		return nil, errors.New("react agent: empty messages")
	}

	var (
		stream *schema.StreamReader[*schema.Message]
		err    error
	)
	if a.inner == nil {
		stream, err = a.chat.Stream(ctx, req.Messages)
	} else {
		stream, err = a.inner.Stream(ctx, req.Messages)
	}
	if err != nil {
		return nil, fmt.Errorf("open stream: %w", err)
	}

	return drainStream(stream, cb)
}

// drainStream 收流并逐块回调。
//
// 中途中止时把已产出的内容一并交回上层：流断了用户也该看到已有的回答，
// 而不是拿到一个空响应。
func drainStream(stream *schema.StreamReader[*schema.Message], cb StreamCallback) (*Response, error) {
	defer stream.Close()

	var sb strings.Builder
	var usage *schema.TokenUsage
	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return &Response{Content: sb.String(), Usage: usage}, fmt.Errorf("stream recv: %w", err)
		}
		usage = pickUsage(usage, msg)
		if len(msg.Content) > 0 {
			sb.WriteString(msg.Content)
			if cb != nil {
				cb(msg.Content)
			}
		}
	}
	return &Response{Content: sb.String(), Usage: usage}, nil
}

// usageOf 取消息上的 token 用量（可能为 nil）。
func usageOf(msg *schema.Message) *schema.TokenUsage {
	if msg == nil || msg.ResponseMeta == nil {
		return nil
	}
	return msg.ResponseMeta.Usage
}

// pickUsage 取 TotalTokens 最大的那份用量：
// 流式下 usage 一般只在最后一个分片出现，中间分片可能只有残缺统计。
func pickUsage(cur *schema.TokenUsage, msg *schema.Message) *schema.TokenUsage {
	u := usageOf(msg)
	if u == nil {
		return cur
	}
	if cur == nil || u.TotalTokens >= cur.TotalTokens {
		return u
	}
	return cur
}
