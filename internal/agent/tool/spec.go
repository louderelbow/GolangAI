package tool

import (
	"context"
	"errors"
	"fmt"
	"time"

	"deeptalk/internal/infra/metrics"

	einotool "github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
)

// InvokeFunc 执行一次工具调用。
// argsJSON 是模型按 ToolInfo 生成的 JSON 字符串，由具体工具自行反序列化。
type InvokeFunc func(ctx context.Context, argsJSON string) (string, error)

// RetryPolicy 工具失败时的重试策略。
//
// 是否允许自动重试取决于工具是否幂等：查天气可以重试，下单不能。
// 因此重试策略挂在工具上，而不是统一在 Agent 循环里决定。
type RetryPolicy struct {
	MaxAttempts int           // 含首次在内最多尝试几次，<=1 表示不重试
	BackoffBase time.Duration // 指数退避基数
	AllowReplan bool          // 失败后是否允许把错误回填、让模型换参数再试
}

// ToolSpec 声明一个工具及其运行语义。
//
// 与 eino 的 Tool 相比，它额外承载超时 / 重试 / 幂等 / 缓存这类"运行策略"，
// 目的是让这些策略和工具实现放在一起，而不是散落在 Agent 循环里逐个特判。
type ToolSpec struct {
	Name        string
	Description string
	Info        *schema.ToolInfo // 为 nil 时按 Name/Description 兜底构造
	Handler     InvokeFunc

	Timeout     time.Duration // 单次调用超时，0 表示沿用外层 ctx
	RetryPolicy RetryPolicy   // 重试策略（PHASE-2 生效）
	Idempotent  bool          // 只有幂等工具才允许自动重试
	SkillName   string        // 归属的 skill（PHASE-8）
	CacheTTL    time.Duration // 结果缓存时长，0 表示不缓存（PHASE-5）
}

// 单次工具调用的默认超时与最大尝试次数。
//
// 之所以做成包级默认而不是每个 ToolSpec 必填：绝大部分工具用同一套策略，
// 逐个填既啰嗦又容易漏。启动时由 Configure 用 [agent] 配置覆盖一次。
var (
	defaultToolTimeout = 15 * time.Second
	defaultMaxAttempts = 2
)

// Configure 在启动装配阶段设置工具的统一默认策略。
// 非正值忽略，避免把零值写进去导致"没有超时"。
func Configure(timeout time.Duration, maxAttempts int) {
	if timeout > 0 {
		defaultToolTimeout = timeout
	}
	if maxAttempts > 0 {
		defaultMaxAttempts = maxAttempts
	}
}

// EffectiveTimeout 返回实际生效的单次调用超时。
// 未显式配置时用统一默认值，避免某个工具卡住整个 Agent 循环。
func (s ToolSpec) EffectiveTimeout() time.Duration {
	if s.Timeout > 0 {
		return s.Timeout
	}
	return defaultToolTimeout
}

// toolInfo 返回给模型看的元信息。
func (s ToolSpec) toolInfo() *schema.ToolInfo {
	if s.Info != nil {
		return s.Info
	}
	return &schema.ToolInfo{Name: s.Name, Desc: s.Description}
}

// specTool 把 ToolSpec 适配成 eino 可调用工具。
type specTool struct {
	spec ToolSpec
}

func (t *specTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return t.spec.toolInfo(), nil
}

func (t *specTool) InvokableRun(ctx context.Context, argumentsInJSON string, opts ...einotool.Option) (string, error) {
	if t.spec.Handler == nil {
		return "", fmt.Errorf("tool %s has no handler", t.spec.Name)
	}

	runCtx, cancel := context.WithTimeout(ctx, t.spec.EffectiveTimeout())
	defer cancel()

	out, err := runWithRetry(runCtx, t.spec, argumentsInJSON)
	if err == nil {
		return out, nil
	}

	// 超时是"这一步没做成"，不是"整轮失败"：把它当成工具的输出回填给模型，
	// 让模型自己决定换参数、换工具，还是如实告诉用户暂时拿不到数据。
	//
	// 直接返回 error 会让 ReAct 循环整轮中断——一个不稳的工具就能让用户
	// 拿不到任何回答，代价远大于收益。这也是 AGENT_SPEC 里
	// "超时作为 observation 回填"的含义。
	if isToolTimeout(runCtx, ctx) {
		metrics.CountAgentTimeout("tool")
		return fmt.Sprintf("工具 %s 调用超时（超过 %s），本次未获得结果。可换一种方式重试，或告知用户暂时无法获取。",
			t.spec.Name, t.spec.EffectiveTimeout()), nil
	}
	return out, err
}

// isToolTimeout 判断是否为"本次工具调用的超时"，
// 以便与"调用方整轮超时"区分开：后者不该被当成工具结果回填。
func isToolTimeout(runCtx, parentCtx context.Context) bool {
	if parentCtx.Err() != nil {
		return false
	}
	return errors.Is(runCtx.Err(), context.DeadlineExceeded)
}

// BaseTool 把 ToolSpec 转成 eino 工具，交给 ReAct Agent 使用。
func (s ToolSpec) BaseTool() einotool.BaseTool {
	return &specTool{spec: s}
}
