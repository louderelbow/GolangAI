package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"deeptalk/internal/agent/guard"
	"deeptalk/internal/infra/metrics"

	"github.com/cloudwego/eino/components/model"
	einotool "github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/flow/agent/react"
	"github.com/cloudwego/eino/schema"
)

// defaultMaxStep 是 ReAct 循环的步数兜底上限。
//
// 真正的预算控制由 guard.Budget 施加（它按模型调用次数计步，且能卡住 token），
// 这里只是给 eino 循环一个自身不会失控的上限，避免两者都不设时无限循环。
const defaultMaxStep = 5

// ReActConfig 构造 ReAct Agent 所需的依赖。
type ReActConfig struct {
	Model   model.ToolCallingChatModel
	Tools   []einotool.BaseTool
	MaxStep int

	// Limits 单轮资源上限（步数 / token / 墙钟 / 单次模型超时）。
	// 零值表示该项不限制。
	Limits guard.Limits
}

// reactAgent 承载"思考 → 调工具 → 观察 → 再思考"循环。
//
// 循环本身复用 eino 的 ReAct 实现；外面这层包壳的价值在于：
// 1) 给上层一个不依赖具体 Agent 框架的稳定接口；
// 2) 把超时与预算统一施加在模型调用层（guard），循环内部无需感知；
// 3) 工具为空时自动退化为普通对话，而不是让调用方特判。
type reactAgent struct {
	chat    model.ToolCallingChatModel
	inner   *react.Agent
	maxStep int
	budget  *guard.Budget
	limits  guard.Limits

	// turnMu 把同一实例上的执行串行化。
	//
	// 存在的唯一理由是保护 budget 的**轮级**语义：每轮入口要把预算清零，
	// 若两轮并发执行，后进的那次会在前一轮跑到一半时把计数抹掉，
	// 于是资源上限静默失效——限流失效比限流误伤更难发现。
	//
	// 上层（service/chat 的 AIHelper）本来就按会话串行，这把锁只是把
	// 那个隐含约束在类型内部也表达出来，行为上没有变化。
	turnMu sync.Mutex
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

	// 预算与超时施加在模型层：循环内部每一步的 token 消耗只有这里看得见。
	//
	// 注意这份预算的生命周期是**轮**，不是这个 Agent 实例：Agent 随会话复用，
	// 而预算每轮都要清零，所以在 Run/Stream 的入口调用 Reset（见下）。
	budget := guard.NewBudget(cfg.Limits)
	chat := guard.WrapModel(cfg.Model, budget)

	a := &reactAgent{
		chat:    chat,
		maxStep: maxStep,
		budget:  budget,
		limits:  cfg.Limits,
	}
	if len(cfg.Tools) == 0 {
		return a, nil
	}

	inner, err := react.NewAgent(ctx, &react.AgentConfig{
		ToolCallingModel: chat,
		ToolsConfig:      compose.ToolsNodeConfig{Tools: cfg.Tools},
		// eino 的 MaxStep 是**单次调用**内的循环上限（一次工具往返算一步），
		// 与 guard.Budget 的 MaxSteps（本轮累计的模型调用次数）是两回事。
		// 前者防 ReAct 跑飞，后者防单轮烧太多 token；两个都要有。
		MaxStep: maxStep,
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

	a.turnMu.Lock()
	defer a.turnMu.Unlock()

	// 预算按轮清零：Agent 随会话复用，不清零就会跨轮累加
	a.budget.Reset()

	turnCtx, cancel := guard.WithTurnTimeout(ctx, a.limits.MaxWallClock)
	defer cancel()

	if a.inner == nil {
		msg, err := a.chat.Generate(turnCtx, req.Messages)
		if err != nil {
			return nil, a.turnErr(turnCtx, err)
		}
		return &Response{Content: msg.Content, Usage: usageOf(msg)}, nil
	}

	msg, err := a.inner.Generate(turnCtx, req.Messages)
	if err != nil {
		return nil, a.turnErr(turnCtx, err)
	}
	return &Response{Content: msg.Content, Usage: usageOf(msg)}, nil
}

// Stream 边生成边回调。
func (a *reactAgent) Stream(ctx context.Context, req Request, cb StreamCallback) (*Response, error) {
	if len(req.Messages) == 0 {
		return nil, errors.New("react agent: empty messages")
	}

	a.turnMu.Lock()
	defer a.turnMu.Unlock()

	// 预算按轮清零（同 Run）：流式一轮可能跨多步，但上限仍是本轮的
	a.budget.Reset()

	turnCtx, cancel := guard.WithTurnTimeout(ctx, a.limits.MaxWallClock)
	defer cancel()

	var (
		stream *schema.StreamReader[*schema.Message]
		err    error
	)
	if a.inner == nil {
		stream, err = a.chat.Stream(turnCtx, req.Messages)
	} else {
		stream, err = a.inner.Stream(turnCtx, req.Messages)
	}
	if err != nil {
		return nil, a.turnErr(turnCtx, err)
	}

	resp, err := drainStream(stream, cb)
	if err != nil {
		return resp, a.turnErr(turnCtx, err)
	}

	// 流式 token 不经过 budgetModel.Generate，这里用同一份预算结算
	if a.budget != nil && resp != nil && resp.Usage != nil {
		if settleErr := a.budget.Settle(resp.Usage); settleErr != nil {
			return resp, settleErr
		}
	}
	return resp, nil
}

// turnErr 把"整轮墙钟耗尽"翻译成统一哨兵错误，
// 上层据此返回已产出的部分内容 + 明确的超时提示，而不是 500。
func (a *reactAgent) turnErr(ctx context.Context, err error) error {
	if ctx.Err() != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		metrics.CountAgentTimeout("turn")
		return fmt.Errorf("%w: %v", guard.ErrTurnTimeout, err)
	}
	return err
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
