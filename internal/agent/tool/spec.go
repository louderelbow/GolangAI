package tool

import (
	"context"
	"errors"
	"fmt"
	"time"

	"deeptalk/internal/agent/trace"
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

	// InterruptOnError 表示这个工具**用错误表达业务语义**：它返回 error
	// 不是失败，而是要求中断本轮。ask_user 就是靠报错让 Agent 停下来问用户。
	//
	// 打开后有三点不同：错误原样上抛、不进熔断器、不回填成 observation。
	// 少了第三点，一个"信息不足"的正常澄清会被当成工具故障；
	// 少了第一点，eino 的 ReAct 循环不会停。
	InterruptOnError bool
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

	// 轨迹入口之二。工具的成功率与耗时都从这里算出来——
	// 而"成功率"必须按下面的分支分别标 ok / timeout / rejected / error，
	// 否则一个模型反复试探越界路径的会话，在仪表盘上会显示成"工具大面积故障"。
	sp := trace.Begin(ctx, trace.KindTool, t.spec.Name)

	// 循环检测：同一个工具 + 完全相同的参数已经成功跑过几次了，就别再跑了。
	//
	// 放在真正执行**之前**：省下的不只是时间，还有一次可能带副作用的调用
	// （将来加 write_file / run_command 时，这一点比省时间重要得多）。
	if g := loopGuardFrom(ctx); g != nil {
		if n, allFailed, blocked := g.Blocked(t.spec.Name, argumentsInJSON); blocked {
			sp.End(trace.StatusRejected, "重复调用，已打断")
			metrics.CountAgentLoopDetected(t.spec.Name)
			notifyLoop(ctx, t.spec.Name, n, allFailed)
			return loopObservation(t.spec.Name, argumentsInJSON, n, allFailed), nil
		}
	}

	runCtx, cancel := context.WithTimeout(ctx, t.spec.EffectiveTimeout())
	defer cancel()

	// 这里刻意**不**套熔断器。
	//
	// 熔断器的用途是"别再去打一个反复失败的远程依赖"。本地工具是进程内代码，
	// 没有这样的依赖；而 resilience.Do 会读全局配置，一旦引进来，本包与
	// core 包的单测就必须先准备一份 config.toml 才能跑——纯粹的循环逻辑
	// 不该背这个负担。
	//
	// 真正会失败的是 MCP 工具（外部进程 / HTTP），它们各自有独立熔断器，
	// 见 mcp_adapter.go。
	out, err := runWithRetry(runCtx, t.spec, argumentsInJSON)
	if err == nil {
		loopGuardFrom(ctx).Succeeded(t.spec.Name, argumentsInJSON)
		sp.EndOK(out)
		return out, nil
	}

	// 下面这些是**真正的失败**，都要计入循环检测。
	//
	// 两个例外不计：InterruptOnError（那是澄清在用错误表达"中断本轮"，
	// 不是失败）和整轮已结束（循环已经收尾了，再记账没有意义）。
	if t.spec.InterruptOnError {
		sp.EndOK("中断本轮以向用户提问")
		return out, err
	}

	if ctx.Err() != nil {
		sp.EndErr(err, "整轮已结束")
		return out, err
	}

	if isToolTimeout(runCtx, ctx) {
		metrics.CountAgentTimeout("tool")
		notify(ctx, t.spec.Name, err)
		sp.End(trace.StatusTimeout, err.Error())
		loopGuardFrom(ctx).Failed(t.spec.Name, argumentsInJSON)
		return fmt.Sprintf("工具 %s 调用超时（超过 %s），本次未获得结果。可换一种方式重试，或告知用户暂时无法获取。",
			t.spec.Name, t.spec.EffectiveTimeout()), nil
	}

	sp.EndErr(err, "")
	loopGuardFrom(ctx).Failed(t.spec.Name, argumentsInJSON)
	return toolFailureObservation(ctx, t.spec.Name, err), nil
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
