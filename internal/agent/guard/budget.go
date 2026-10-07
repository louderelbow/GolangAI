package guard

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"deeptalk/internal/agent/trace"
	"deeptalk/internal/infra/metrics"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// Budget 在一次 Agent 执行（**一轮对话**）中累计并校验资源用量。
//
// 为什么装饰模型而不是装饰 Agent：ReAct 循环每一步的 token 消耗只在模型调用
// 这一层可见，包在 Agent 外面只能做事后统计，无法真正"卡住"。
//
// ⚠️ 生命周期是**轮级**，不是 Agent 级：被预算包装的模型是在构造 Agent 时
// 固定下来的，而 Agent 随会话复用（service/chat 里每个会话一个）。所以每轮
// 开始前必须调用 Reset，否则 steps/tokens/deadline 会跨轮累加——
// 会话累计跑满 MaxSteps 次模型调用之后，之后每一轮都会直接失败。
// （这个坑真实发生过：maxSteps=8 被当成"整个会话 8 次调用"。）
type Budget struct {
	lim Limits

	mu       sync.Mutex
	steps    int
	tokens   int
	deadline time.Time
}

// NewBudget 按上限构造一份**新一轮**的预算。
func NewBudget(lim Limits) *Budget {
	b := &Budget{lim: lim}
	b.Reset()
	return b
}

// Reset 开启新一轮：清零步数与 token，并按当前时刻重新武装墙钟。
//
// 必须在每轮 Agent 执行的入口调用。Reset 只应在"没有正在进行的轮次"时调用——
// internal/agent/core 用一把轮级互斥锁保证这一点。
func (b *Budget) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.steps = 0
	b.tokens = 0
	if b.lim.MaxWallClock > 0 {
		b.deadline = time.Now().Add(b.lim.MaxWallClock)
	} else {
		b.deadline = time.Time{}
	}
}

// Exceeded 判断错误是否由预算耗尽引起。
func Exceeded(err error) bool { return errors.Is(err, ErrBudgetExceeded) }

// beforeCall 在一次模型调用之前校验步数与墙钟，并计入一步。
func (b *Budget) beforeCall() error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.lim.MaxSteps > 0 && b.steps >= b.lim.MaxSteps {
		metrics.CountAgentBudgetExceeded("steps")
		return fmt.Errorf("%w: steps=%d", ErrBudgetExceeded, b.steps)
	}
	if !b.deadline.IsZero() && time.Now().After(b.deadline) {
		metrics.CountAgentBudgetExceeded("wallclock")
		return fmt.Errorf("%w: wallclock", ErrBudgetExceeded)
	}
	b.steps++
	return nil
}

// afterCall 用真实 usage 累加 token 用量。
func (b *Budget) afterCall(u *schema.TokenUsage) error {
	if u == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.tokens += u.TotalTokens
	if b.lim.MaxTokens > 0 && b.tokens > b.lim.MaxTokens {
		metrics.CountAgentBudgetExceeded("tokens")
		return fmt.Errorf("%w: tokens=%d>%d", ErrBudgetExceeded, b.tokens, b.lim.MaxTokens)
	}
	return nil
}

// Steps 返回已消耗步数。
func (b *Budget) Steps() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.steps
}

// Tokens 返回已消耗 token 数。
func (b *Budget) Tokens() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.tokens
}

// budgetModel 把预算施加到模型调用上。
type budgetModel struct {
	inner model.ToolCallingChatModel
	b     *Budget
	lim   Limits
}

// WrapModel 用预算约束一个模型；inner 或 b 为 nil 时原样返回。
func WrapModel(inner model.ToolCallingChatModel, b *Budget) model.ToolCallingChatModel {
	if inner == nil || b == nil {
		return inner
	}
	return &budgetModel{inner: inner, b: b, lim: b.lim}
}

// WithTools 绑定工具后仍要带上同一份预算，
// 否则工具分支上的模型调用会绕开预算统计。
func (m *budgetModel) WithTools(tools []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	next, err := m.inner.WithTools(tools)
	if err != nil {
		return nil, err
	}
	return &budgetModel{inner: next, b: m.b, lim: m.lim}, nil
}

func (m *budgetModel) Generate(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	// 轨迹入口之一。这里是 Agent 每一次"想"的地方，所以 ReAct 步数
	// 就是这里被经过的次数——指标不用额外统计，从轨迹里数得出来。
	sp := trace.Begin(ctx, trace.KindModel, "generate")

	// 预算耗尽不是故障，是策略：模型想再试一次，但这一轮已经用完了额度。
	// 标 rejected 而不是 error，否则预算生效会让模型成功率看起来像在故障。
	if err := m.b.beforeCall(); err != nil {
		sp.End(trace.StatusRejected, err.Error())
		return nil, err
	}

	callCtx, cancel := WithModelTimeout(ctx, m.lim.ModelTimeout)
	defer cancel()

	msg, err := m.inner.Generate(callCtx, input, opts...)
	if err != nil {
		// 单次模型调用超时（区别于整轮超时：外层 ctx 还没结束）
		if callCtx.Err() != nil && ctx.Err() == nil {
			metrics.CountAgentTimeout("model")
		}
		sp.EndErr(err, "")
		return nil, err
	}
	if err := m.b.afterCall(usageOf(msg)); err != nil {
		sp.End(trace.StatusRejected, err.Error())
		return nil, err
	}
	sp.EndOK(traceDetail(msg))
	return msg, nil
}

func (m *budgetModel) Stream(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	sp := trace.Begin(ctx, trace.KindModel, "stream")

	if err := m.b.beforeCall(); err != nil {
		sp.End(trace.StatusRejected, err.Error())
		return nil, err
	}

	// 流式不在这里套单次超时：流要一直读到结束，提前 cancel 会截断回答。
	// 流的总时长由整轮墙钟上限约束，token 结算由收流层用同一份 Budget 完成。
	stream, err := m.inner.Stream(ctx, input, opts...)
	if err != nil {
		sp.EndErr(err, "")
		return nil, err
	}
	// 流式只记到"建立连接"为止：真正的耗时在收流层，
	// 把首包时间当成整段生成时间会让指标低得毫无意义。
	sp.EndOK("已建立流")
	return stream, nil
}

// traceDetail 给模型调用补一句可读的注脚。
//
// 轨迹是给人看的，所以这里尽量写"这一步发生了什么"：
// 是要调工具，还是给出了最终答案。事后翻轨迹时，这一句比时间戳有用得多。
func traceDetail(msg *schema.Message) string {
	if msg == nil {
		return ""
	}
	if n := len(msg.ToolCalls); n > 0 {
		names := make([]string, 0, n)
		for _, tc := range msg.ToolCalls {
			names = append(names, tc.Function.Name)
		}
		return "请求工具: " + strings.Join(names, ", ")
	}
	if msg.Content != "" {
		return "给出回答"
	}
	return ""
}

// Settle 让收流层把流式 usage 结算进预算。
func (b *Budget) Settle(u *schema.TokenUsage) error { return b.afterCall(u) }

// usageOf 取消息上的 token 用量（可能为 nil）。
func usageOf(msg *schema.Message) *schema.TokenUsage {
	if msg == nil || msg.ResponseMeta == nil {
		return nil
	}
	return msg.ResponseMeta.Usage
}
