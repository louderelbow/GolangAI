package tool

import (
	"context"
	"fmt"
	"time"

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

// EffectiveTimeout 返回实际生效的单次调用超时。
// 未显式配置时给一个保守默认值，避免某个工具卡住整个 Agent 循环。
const defaultToolTimeout = 15 * time.Second

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
	return t.spec.Handler(runCtx, argumentsInJSON)
}

// BaseTool 把 ToolSpec 转成 eino 工具，交给 ReAct Agent 使用。
func (s ToolSpec) BaseTool() einotool.BaseTool {
	return &specTool{spec: s}
}
