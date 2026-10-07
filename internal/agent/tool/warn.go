// Package tool 定义 Agent 可调用工具的统一契约与运行策略。
//
// 工具在这里被当成"有运行语义的依赖"，而不只是几个函数：超时 / 重试 /
// 幂等 / 熔断 / 失败回填 / 告警都挂在 ToolSpec 上由本包统一施加，
// 而不是散落在 Agent 循环里逐个特判。
//
// 本文件负责其中最后一件：工具失败时除了回填给模型，还要让**用户**知道
// 这次的回答可能不完整。回填的 observation 只有模型看得见，用户看到的
// 是一个照常输出的回答——他无从分辨"没查到"和"查到了但没有"，
// 而这个区别在制度问答这类场景里往往很关键。
package tool

import (
	"context"
	"log"
	"sync"

	"deeptalk/internal/infra/resilience"
)

// maxWarnings 单轮最多上报几条告警。
//
// 工具连环失败时，每条都弹一个提示会把用户刷爆，而且它们通常是同一个根因。
// 真正有价值的信息是"这一步没做成"，说三遍就够了。
const maxWarnings = 3

// Warn 一条要展示给用户的工具告警。
type Warn struct {
	Tool    string // 出问题的工具名
	Message string // 给用户看的一句话
}

// WarnCollector 收集**本轮**的工具告警。
//
// 生命周期与"一轮对话"严格一致：由服务层在开始一轮前放进 ctx，一轮结束后取走。
// 与 askuser.Collector 是同一套路——工具层只负责写，服务层负责取，
// 于是 agent/tool 不必知道 HTTP、SSE 或前端的任何事。
type WarnCollector struct {
	mu   sync.Mutex
	seen map[string]bool
	list []Warn
}

func NewWarnCollector() *WarnCollector {
	return &WarnCollector{seen: make(map[string]bool)}
}

// Add 记一条告警。同一个工具的同一条文案只记一次，总数封顶。
//
// 接收者可能是 nil（本轮没放收集器），此时静默丢弃——
// 让调用方随处调用而不必判空。
func (c *WarnCollector) Add(w Warn) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	if len(c.list) >= maxWarnings {
		return
	}
	key := w.Tool + "\x00" + w.Message
	if c.seen[key] {
		return
	}
	c.seen[key] = true
	c.list = append(c.list, w)
}

// List 返回已收集告警的副本。
func (c *WarnCollector) List() []Warn {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Warn, len(c.list))
	copy(out, c.list)
	return out
}

type warnKey struct{}

// WithWarnings 把收集器放进 ctx（由服务层在每轮开始时调用）。
func WithWarnings(ctx context.Context, c *WarnCollector) context.Context {
	if c == nil {
		return ctx
	}
	return context.WithValue(ctx, warnKey{}, c)
}

// WarningsFrom 取本轮收集到的告警（服务层用）。
func WarningsFrom(ctx context.Context) []Warn {
	c, ok := ctx.Value(warnKey{}).(*WarnCollector)
	if !ok {
		return nil
	}
	return c.List()
}

// toolFailureObservation 记一次告警，并把失败变成给模型的 observation。
//
// 这是本项目的既定原则：工具这一步没做成，不等于整轮失败。模型看到
// observation 之后可以换参数、换工具，或者如实告诉用户拿不到数据；
// 直接返回 error 会让 eino 的 ToolsNode 中断整个 graph，本轮回答全丢。
func toolFailureObservation(ctx context.Context, toolName string, err error) string {
	notify(ctx, toolName, err)

	if resilience.IsOpen(err) {
		return "工具 " + toolName + " 暂时不可用（连续失败已触发熔断），本次未获得结果。" +
			"不要再重复调用本工具，请直接告知用户暂时拿不到这部分数据。"
	}
	return "工具 " + toolName + " 调用失败：" + err.Error() + "。" +
		"本次未获得结果，可以换一种参数或方式重试；若仍失败，请如实告知用户暂时无法获取。"
}

// notify 记日志 + 收集一条给用户看的告警。
func notify(ctx context.Context, toolName string, err error) {
	log.Printf("[tool] %s failed: %v", toolName, err)

	c, _ := ctx.Value(warnKey{}).(*WarnCollector)
	c.Add(Warn{
		Tool:    toolName,
		Message: "「" + toolName + "」这次没能取到数据，回答可能不完整",
	})
}
