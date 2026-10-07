package core_test

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	agentcore "deeptalk/internal/agent/core"
	agenttool "deeptalk/internal/agent/tool"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// ==================== 测试替身 ====================

type fakeState struct {
	generateCalls int32
	toolCalls     int32
	boundTools    []*schema.ToolInfo
}

// fakeToolModel 模拟"先发起工具调用、拿到结果后再给最终答案"的模型，
// 用来验证 ReAct 循环真的跑起来了（而不是只调了一次模型就返回）。
type fakeToolModel struct{ st *fakeState }

func (m *fakeToolModel) WithTools(tools []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	m.st.boundTools = tools
	return m, nil
}

func (m *fakeToolModel) Generate(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	atomic.AddInt32(&m.st.generateCalls, 1)

	// 上下文里已经带了工具结果 → 给出最终答案，循环结束
	for _, msg := range input {
		if msg.Role == schema.Tool {
			return &schema.Message{Role: schema.Assistant, Content: "最终答案"}, nil
		}
	}

	// 否则发起一次工具调用
	atomic.AddInt32(&m.st.toolCalls, 1)
	return &schema.Message{
		Role: schema.Assistant,
		ToolCalls: []schema.ToolCall{{
			ID:       "call-1",
			Type:     "function",
			Function: schema.FunctionCall{Name: "fake_echo", Arguments: `{"q":"hi"}`},
		}},
	}, nil
}

func (m *fakeToolModel) Stream(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	msg, err := m.Generate(ctx, input, opts...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{msg}), nil
}

// plainModel 不调用任何工具，只回一句话。
type plainModel struct{ calls int32 }

func (m *plainModel) WithTools([]*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return m, nil
}

func (m *plainModel) Generate(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	atomic.AddInt32(&m.calls, 1)
	return &schema.Message{Role: schema.Assistant, Content: "纯对话回答"}, nil
}

func (m *plainModel) Stream(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	msg, err := m.Generate(ctx, input, opts...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{msg}), nil
}

// ==================== 用例 ====================

// TestReActAgentInvokesRegisteredTool 是 PHASE-1 的核心验收：
// 注册一个假工具，Agent 必须真的调用它，并把工具结果喂回模型拿到最终答案。
func TestReActAgentInvokesRegisteredTool(t *testing.T) {
	var invoked int32
	reg := agenttool.NewRegistry()
	if err := reg.Register(agenttool.ToolSpec{
		Name:        "fake_echo",
		Description: "回声工具",
		Handler: func(ctx context.Context, args string) (string, error) {
			atomic.AddInt32(&invoked, 1)
			return "tool-result", nil
		},
		Idempotent: true,
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
		SessionID: "s1",
		UserName:  "tester",
		Messages:  []*schema.Message{{Role: schema.User, Content: "你好"}},
	})
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}

	if got := atomic.LoadInt32(&invoked); got != 1 {
		t.Fatalf("Agent 应调用假工具 1 次，实际 %d 次（ReAct 循环没跑起来）", got)
	}
	if resp.Content != "最终答案" {
		t.Fatalf("最终回答不对: %q", resp.Content)
	}
	if len(st.boundTools) != 1 || st.boundTools[0].Name != "fake_echo" {
		t.Fatalf("工具应被绑定给模型，实际: %+v", st.boundTools)
	}
	if atomic.LoadInt32(&st.generateCalls) < 2 {
		t.Fatalf("应至少调用模型 2 次（发起工具调用 + 生成最终答案），实际 %d 次", st.generateCalls)
	}
}

// TestReActAgentStreamInvokesTool 流式路径同样要跑通工具调用。
func TestReActAgentStreamInvokesTool(t *testing.T) {
	var invoked int32
	reg := agenttool.NewRegistry()
	if err := reg.Register(agenttool.ToolSpec{
		Name: "fake_echo",
		Handler: func(ctx context.Context, args string) (string, error) {
			atomic.AddInt32(&invoked, 1)
			return "tool-result", nil
		},
	}); err != nil {
		t.Fatalf("注册工具失败: %v", err)
	}

	agent, err := agentcore.NewReActAgent(context.Background(), agentcore.ReActConfig{
		Model:   &fakeToolModel{st: &fakeState{}},
		Tools:   reg.Tools(context.Background()),
		MaxStep: 3,
	})
	if err != nil {
		t.Fatalf("构造 Agent 失败: %v", err)
	}

	var sb strings.Builder
	resp, err := agent.Stream(context.Background(), agentcore.Request{
		Messages: []*schema.Message{{Role: schema.User, Content: "你好"}},
	}, func(chunk string) { sb.WriteString(chunk) })
	if err != nil {
		t.Fatalf("Stream 失败: %v", err)
	}

	if got := atomic.LoadInt32(&invoked); got != 1 {
		t.Fatalf("流式路径也应调用工具 1 次，实际 %d 次", got)
	}
	if resp.Content != "最终答案" || sb.String() != "最终答案" {
		t.Fatalf("流式内容不对: resp=%q 回调累计=%q", resp.Content, sb.String())
	}
}

// TestReActAgentWithoutToolsDegradesToChat 没有工具时要退化为普通对话而不是报错。
func TestReActAgentWithoutToolsDegradesToChat(t *testing.T) {
	m := &plainModel{}
	agent, err := agentcore.NewReActAgent(context.Background(), agentcore.ReActConfig{Model: m})
	if err != nil {
		t.Fatalf("构造 Agent 失败: %v", err)
	}

	resp, err := agent.Run(context.Background(), agentcore.Request{
		Messages: []*schema.Message{{Role: schema.User, Content: "你好"}},
	})
	if err != nil {
		t.Fatalf("无工具时应正常返回，实际报错: %v", err)
	}
	if resp.Content != "纯对话回答" {
		t.Fatalf("回答不对: %q", resp.Content)
	}
	if atomic.LoadInt32(&m.calls) != 1 {
		t.Fatalf("应只调用模型 1 次，实际 %d 次", m.calls)
	}
}

// TestReActAgentRejectsEmptyMessages 空输入要明确报错，而不是让循环空转。
func TestReActAgentRejectsEmptyMessages(t *testing.T) {
	agent, err := agentcore.NewReActAgent(context.Background(), agentcore.ReActConfig{Model: &plainModel{}})
	if err != nil {
		t.Fatalf("构造 Agent 失败: %v", err)
	}
	if _, err := agent.Run(context.Background(), agentcore.Request{}); err == nil {
		t.Fatal("空消息应返回错误")
	}
}

// TestNewReActAgentRequiresModel 缺模型必须构造失败。
func TestNewReActAgentRequiresModel(t *testing.T) {
	if _, err := agentcore.NewReActAgent(context.Background(), agentcore.ReActConfig{}); err == nil {
		t.Fatal("没有模型时应构造失败")
	}
}
