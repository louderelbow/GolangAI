package guard

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"deeptalk/internal/infra/metrics"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// Budget 在一次 Agent 执行过程中累计并校验资源用量。
//
// 为什么装饰模型而不是装饰 Agent：ReAct 循环每一步的 token 消耗只在模型调用
// 这一层可见，包在 Agent 外面只能做事后统计，无法真正"卡住"。
type Budget struct {
	lim Limits

	mu       sync.Mutex
	steps    int
	tokens   int
	deadline time.Time
}

// NewBudget 按上限构造预算；MaxWallClock 从构造时刻开始计时。
func NewBudget(lim Limits) *Budget {
	b := &Budget{lim: lim}
	if lim.MaxWallClock > 0 {
		b.deadline = time.Now().Add(lim.MaxWallClock)
	}
	return b
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
	if err := m.b.beforeCall(); err != nil {
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
		return nil, err
	}
	if err := m.b.afterCall(usageOf(msg)); err != nil {
		return nil, err
	}
	return msg, nil
}

func (m *budgetModel) Stream(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	if err := m.b.beforeCall(); err != nil {
		return nil, err
	}

	// 流式不在这里套单次超时：流要一直读到结束，提前 cancel 会截断回答。
	// 流的总时长由整轮墙钟上限约束，token 结算由收流层用同一份 Budget 完成。
	stream, err := m.inner.Stream(ctx, input, opts...)
	if err != nil {
		return nil, err
	}
	return stream, nil
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
