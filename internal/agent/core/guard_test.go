package core_test

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	agentcore "deeptalk/internal/agent/core"
	"deeptalk/internal/agent/guard"
	agenttool "deeptalk/internal/agent/tool"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// usageModel 每次调用都上报固定的 token 用量，用来验证 token 预算。
type usageModel struct {
	tokens int
	calls  int32
}

func (m *usageModel) WithTools([]*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return m, nil
}

func (m *usageModel) Generate(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	atomic.AddInt32(&m.calls, 1)
	return &schema.Message{
		Role:         schema.Assistant,
		Content:      "回答",
		ResponseMeta: &schema.ResponseMeta{Usage: &schema.TokenUsage{TotalTokens: m.tokens}},
	}, nil
}

func (m *usageModel) Stream(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	msg, err := m.Generate(ctx, input, opts...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{msg}), nil
}

// TestToolTimeoutDoesNotAbortTurn 验收：注入一个必然超时的工具，
// 整轮仍应正常结束并把超时信息作为工具结果回填给模型。
func TestToolTimeoutDoesNotAbortTurn(t *testing.T) {
	var invoked int32

	reg := agenttool.NewRegistry()
	if err := reg.Register(agenttool.ToolSpec{
		Name:    "fake_echo",
		Timeout: 50 * time.Millisecond,
		// 故意不设 Idempotent：超时的非幂等工具不该被自动重试，否则雪上加霜
		Handler: func(ctx context.Context, args string) (string, error) {
			atomic.AddInt32(&invoked, 1)
			<-ctx.Done()
			return "", fmt.Errorf("tool timed out: %w", ctx.Err())
		},
	}); err != nil {
		t.Fatalf("注册工具失败: %v", err)
	}

	st := &fakeState{}
	agent, err := agentcore.NewReActAgent(context.Background(), agentcore.ReActConfig{
		Model:   &fakeToolModel{st: st},
		Tools:   reg.Tools(context.Background()),
		MaxStep: 3,
	})
	if err != nil {
		t.Fatalf("构造 Agent 失败: %v", err)
	}

	resp, err := agent.Run(context.Background(), agentcore.Request{
		Messages: []*schema.Message{{Role: schema.User, Content: "你好"}},
	})
	if err != nil {
		t.Fatalf("工具超时不应中断整轮，实际报错: %v", err)
	}
	if resp.Content != "最终答案" {
		t.Fatalf("应拿到模型的最终回答，实际: %q", resp.Content)
	}
	if got := atomic.LoadInt32(&invoked); got != 1 {
		t.Fatalf("非幂等的超时工具不应被重试，实际调用 %d 次", got)
	}
	if st.toolCalls != 1 {
		t.Fatalf("模型应只发起 1 次工具调用，实际 %d 次", st.toolCalls)
	}
}

// TestBudgetStopsOnMaxSteps 验收：步数触顶必须停止，而不是继续循环。
//
// 这里用一个"永远只想调工具、从不给最终答案"的模型，
// 保证循环只能靠预算终止。
func TestBudgetStopsOnMaxSteps(t *testing.T) {
	reg := agenttool.NewRegistry()
	if err := reg.Register(agenttool.ToolSpec{
		Name: "fake_echo",
		Handler: func(context.Context, string) (string, error) {
			return "ok", nil
		},
	}); err != nil {
		t.Fatalf("注册工具失败: %v", err)
	}

	m := &loopingModel{}
	agent, err := agentcore.NewReActAgent(context.Background(), agentcore.ReActConfig{
		Model:   m,
		Tools:   reg.Tools(context.Background()),
		MaxStep: 8, // eino 自身放宽，让预算成为唯一约束
		Limits:  guard.Limits{MaxSteps: 2},
	})
	if err != nil {
		t.Fatalf("构造 Agent 失败: %v", err)
	}

	_, err = agent.Run(context.Background(), agentcore.Request{
		Messages: []*schema.Message{{Role: schema.User, Content: "你好"}},
	})
	if !guard.Exceeded(err) {
		t.Fatalf("步数超限应返回 ErrBudgetExceeded，实际: %v", err)
	}
	if got := atomic.LoadInt32(&m.calls); got > 3 {
		t.Fatalf("预算触顶后不应继续调用模型，实际调用 %d 次", got)
	}
}

// loopingModel 永远返回工具调用，从不给最终答案。
type loopingModel struct{ calls int32 }

func (m *loopingModel) WithTools([]*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return m, nil
}

func (m *loopingModel) Generate(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	atomic.AddInt32(&m.calls, 1)
	return &schema.Message{
		Role: schema.Assistant,
		ToolCalls: []schema.ToolCall{{
			ID:       "call-loop",
			Type:     "function",
			Function: schema.FunctionCall{Name: "fake_echo", Arguments: `{}`},
		}},
	}, nil
}

func (m *loopingModel) Stream(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	msg, err := m.Generate(ctx, input, opts...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{msg}), nil
}

// TestBudgetStopsOnMaxTokens 验收：token 触顶必须停止。
func TestBudgetStopsOnMaxTokens(t *testing.T) {
	m := &usageModel{tokens: 200}
	agent, err := agentcore.NewReActAgent(context.Background(), agentcore.ReActConfig{
		Model:  m,
		Limits: guard.Limits{MaxTokens: 100},
	})
	if err != nil {
		t.Fatalf("构造 Agent 失败: %v", err)
	}

	_, err = agent.Run(context.Background(), agentcore.Request{
		Messages: []*schema.Message{{Role: schema.User, Content: "你好"}},
	})
	if !guard.Exceeded(err) {
		t.Fatalf("token 超限应返回 ErrBudgetExceeded，实际: %v", err)
	}
	if got := atomic.LoadInt32(&m.calls); got != 1 {
		t.Fatalf("超限后不应再调用模型，实际调用 %d 次", got)
	}
}

// TestBudgetNotExceededWithinLimits 上限之内不应误报。
func TestBudgetNotExceededWithinLimits(t *testing.T) {
	m := &usageModel{tokens: 50}
	agent, err := agentcore.NewReActAgent(context.Background(), agentcore.ReActConfig{
		Model:  m,
		Limits: guard.Limits{MaxTokens: 100, MaxSteps: 3},
	})
	if err != nil {
		t.Fatalf("构造 Agent 失败: %v", err)
	}

	resp, err := agent.Run(context.Background(), agentcore.Request{
		Messages: []*schema.Message{{Role: schema.User, Content: "你好"}},
	})
	if err != nil {
		t.Fatalf("未超限不应报错: %v", err)
	}
	if resp.Content != "回答" {
		t.Fatalf("回答不对: %q", resp.Content)
	}
}

// TestTurnTimeoutReturnsSentinel 整轮墙钟耗尽应返回可识别的哨兵错误，
// 上层据此返回部分内容而不是 500。
func TestTurnTimeoutReturnsSentinel(t *testing.T) {
	agent, err := agentcore.NewReActAgent(context.Background(), agentcore.ReActConfig{
		Model:  &slowModel{delay: 500 * time.Millisecond},
		Limits: guard.Limits{MaxWallClock: 40 * time.Millisecond},
	})
	if err != nil {
		t.Fatalf("构造 Agent 失败: %v", err)
	}

	_, err = agent.Run(context.Background(), agentcore.Request{
		Messages: []*schema.Message{{Role: schema.User, Content: "你好"}},
	})
	if err == nil {
		t.Fatal("整轮超时应返回错误")
	}
	if !guard.TurnTimedOut(err) {
		t.Fatalf("应返回可识别的超时错误，实际: %v", err)
	}
}

// slowModel 模拟上游很慢。
type slowModel struct{ delay time.Duration }

func (m *slowModel) WithTools([]*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return m, nil
}

func (m *slowModel) Generate(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(m.delay):
		return &schema.Message{Role: schema.Assistant, Content: "慢回答"}, nil
	}
}

func (m *slowModel) Stream(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	msg, err := m.Generate(ctx, input, opts...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{msg}), nil
}
