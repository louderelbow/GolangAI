package tool

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
)

// ======================== 循环检测 ========================
//
// 识别"模型用完全相同的参数反复调用同一个工具"。
//
// 为什么需要它：实测过一次 —— 用户只是问"列出工作区文件"，qwen-turbo 连着
// 调了 4 次 list_files({"path":"."})，每次都成功、每次耗时 2ms，最后撞满
// maxStep 抛出一句 "exceeds max steps"。用户看到的是一个技术错误，
// 而真实情况是"模型在原地空转"。
//
// 工具成功率 100%、工具耗时 2ms —— 从任何单个指标上看都完全健康。
// 只有把一轮里所有调用摆在一起（也就是看轨迹）才能发现它在重复。
//
// 设计上有三条边界，每一条都是为了不与其它机制打架：
//
//  1. **只对成功的调用计数**。工具超时/报错之后模型重试同样的参数完全是
//     合理的，把它当循环会误伤。
//
//  2. **"一直失败一直重试"不归这里管**，那是预算（步数上限）的职责。
//     两个机制各有边界，重叠的部分只会互相掩盖。
//
//  3. **拦下来是回填 observation，不是返回 error**。与工具失败回填同一原则：
//     模型看到"你已经试过了"通常会自己换路，整轮继续；
//     直接抛错则本轮回答全丢，用户一无所获。

// DefaultLoopThreshold 同一个 (工具 + 参数) 成功多少次后开始拦截。
//
// 取 3 而不是 2：有些工具合法地会被调用两次（例如"先看目录再确认一遍"），
// 第 3 次之后基本可以确定是死循环。
const DefaultLoopThreshold = 3

// LoopGuard 统计**本轮**内每个 (工具, 参数) 组合被调用了几次、其中失败几次。
//
// 生命周期与一轮对话严格一致，和 WarnCollector / askuser.Collector 同一套路：
// 服务层放进 ctx，工具层只负责读写。
type LoopGuard struct {
	threshold int

	mu   sync.Mutex
	hits map[string]loopStat
}

// loopStat 一个 (工具, 参数) 组合的调用统计。
//
// 单独记 fails 是为了在拦截时能说出**是哪一种循环**：
// "成功 3 次还再来" 和 "失败 3 次还再来" 该给模型不同的话，
// 混成一个数字就只能给一句模糊的提示。
type loopStat struct {
	attempts int
	fails    int
}

// NewLoopGuard 创建守卫；threshold <= 0 时用默认值。
func NewLoopGuard(threshold int) *LoopGuard {
	if threshold <= 0 {
		threshold = DefaultLoopThreshold
	}
	return &LoopGuard{threshold: threshold, hits: make(map[string]loopStat)}
}

// Blocked 在**调用之前**问：这个组合还能调吗？
//
// 返回已经调用过的次数，以及其中失败了几次，便于把原因写清楚给模型看。
func (g *LoopGuard) Blocked(tool, argsJSON string) (attempts int, allFailed bool, blocked bool) {
	if g == nil {
		return 0, false, false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	st := g.hits[g.fingerprint(tool, argsJSON)]
	return st.attempts, st.attempts > 0 && st.fails == st.attempts, st.attempts >= g.threshold
}

// Succeeded 在调用**成功之后**记账。
func (g *LoopGuard) Succeeded(tool, argsJSON string) { g.count(tool, argsJSON, false) }

// Failed 在调用**失败之后**记账。
//
// 失败也计数 —— 这一条是被追问出来的，原来的设计只计成功，理由是
// "失败后重试同样的参数是合理的"。那个理由站不住：
//
//	瞬时错误的重试已经在**工具内部**做完了（RetryPolicy / toolMaxAttempts，
//	默认 2 次）。失败能冒到模型面前，说明重试已经用尽 ——
//	再让模型用完全相同的参数试一次，纯属浪费一轮推理。
//
// 只计成功的后果是：失败循环只能靠预算（步数上限）兜底，而它的结局
// 是 "exceeds max steps" —— 一个技术错误，用户什么都没拿到。
// 和成功循环一样烂，只是换了个失败方式。
func (g *LoopGuard) Failed(tool, argsJSON string) { g.count(tool, argsJSON, true) }

func (g *LoopGuard) count(tool, argsJSON string, failed bool) {
	if g == nil {
		return
	}
	fp := g.fingerprint(tool, argsJSON)
	g.mu.Lock()
	st := g.hits[fp]
	st.attempts++
	if failed {
		st.fails++
	}
	g.hits[fp] = st
	g.mu.Unlock()
}

// fingerprint 把 (工具, 参数) 压成一个定长指纹。
//
// 参数必须**规范化**再哈希：{"a":1,"b":2} 与 {"b":2,"a":1} 是同一次调用，
// 但字节不同。做法是解析成 map 后再序列化 —— Go 的 map 序列化按键排序，
// 天然得到规范形式。
//
// 用哈希而不是原文当键：write_file 的参数可能有几十 KB，
// 一轮里存几十份原文是没必要的内存占用。
func (g *LoopGuard) fingerprint(tool, argsJSON string) string {
	canonical := argsJSON
	var v any
	if err := json.Unmarshal([]byte(argsJSON), &v); err == nil {
		if b, err := json.Marshal(v); err == nil {
			canonical = string(b)
		}
	}
	sum := sha256.Sum256([]byte(tool + "\x00" + canonical))
	return tool + ":" + hex.EncodeToString(sum[:8])
}

// ======================== ctx 传递 ========================

type loopKey struct{}

// WithLoopGuard 把守卫放进 ctx（服务层每轮开始时调用）。
func WithLoopGuard(ctx context.Context, g *LoopGuard) context.Context {
	if g == nil {
		return ctx
	}
	return context.WithValue(ctx, loopKey{}, g)
}

// loopGuardFrom 取本轮的守卫；没放就返回 nil（调用方无需判空，方法对 nil 安全）。
func loopGuardFrom(ctx context.Context) *LoopGuard {
	g, _ := ctx.Value(loopKey{}).(*LoopGuard)
	return g
}

// ======================== 拦截之后说什么 ========================

// maxArgsInMessage 写进 observation 的参数上限。
//
// write_file 之类的参数可能有几十 KB，把它整段塞回给模型纯粹是浪费上下文 ——
// 模型需要认出"是同一个参数"，不需要重读一遍内容。
const maxArgsInMessage = 120

// loopObservation 把"你在重复"变成给模型的 observation。
//
// 关键是**给出路**而不是只报错：明确告诉它可选的三条路。只说"不许重复调用"
// 的话，弱模型会不知所措，继续空转。
//
// 成功循环和失败循环要给**不同的话**：
//   - 成功过 3 次还再来 → "结果不会再变"
//   - 失败过 3 次还再来 → "同样的参数不会改变结果，先查清原因或直接告诉用户"
//
// 混成一句的话，模型会误以为"失败是因为还没成功，再试一次就好"。
func loopObservation(tool, argsJSON string, attempts int, allFailed bool) string {
	args := truncateArgs(argsJSON)

	if allFailed {
		return fmt.Sprintf(
			"工具 %s 已经用完全相同的参数失败了 %d 次（参数：%s）。\n"+
				"同样的参数再试一次不会改变结果。请换一个参数、换一个工具，"+
				"或者如实告诉用户这部分数据暂时拿不到。",
			tool, attempts, args)
	}

	return fmt.Sprintf(
		"工具 %s 已经用完全相同的参数成功调用过 %d 次（参数：%s）。\n"+
			"重复调用不会得到任何新结果。请三选一：换一个参数、换一个工具，或者直接基于已有信息作答。",
		tool, attempts, args)
}

// truncateArgs 截断参数用于展示。
func truncateArgs(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "(空)"
	}
	if len(s) <= maxArgsInMessage {
		return s
	}
	return s[:maxArgsInMessage] + "…"
}

// notifyLoop 记一条给用户看的告警。
//
// 为什么用户也该知道：Agent 打断了自己的死循环，最终回答可能只基于前几次的
// 结果。用户有权知道"它绕了个弯"，或者"它没能拿到这部分数据"。
func notifyLoop(ctx context.Context, tool string, attempts int, allFailed bool) {
	log.Printf("[tool] loop detected: %s called %d times with identical args (allFailed=%v), blocking",
		tool, attempts, allFailed)

	msg := "Agent 在重复调用同一个工具，已打断这次重复"
	if allFailed {
		msg = "「" + tool + "」反复失败，Agent 已停止重试，这部分数据可能拿不到"
	}
	c, _ := ctx.Value(warnKey{}).(*WarnCollector)
	c.Add(Warn{Tool: tool, Message: msg})
}
