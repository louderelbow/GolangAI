// Package askuser 实现"澄清式追问"：Agent 在信息不足时主动向用户提问，
// 并给出可选项，用户选完再继续。
//
// 解决的问题：模型在缺少关键信息时只能靠猜。比如用户说"帮我请假"，
// 而请假制度对年假/病假/事假的规定完全不同——猜错就是编造。
// 与其硬答，不如把缺的那一格问清楚。
//
// 为什么做成工具而不是"生成前先判定一次"：
//  1. 只有 Agent 自己知道当前这一步是否真的缺信息，多一次独立判定调用
//     等于每条消息都付一次钱，而多数消息并不需要澄清；
//  2. 澄清是 Agent 决策链上的一环，落在轨迹里才说得清"它为什么停下来问"。
//
// 本轮如何"停下来等用户"：
//
//	工具执行时把提问写进本轮收集器，然后返回哨兵错误 —— eino 的 ReAct
//	在工具报错时会中断整轮。Agent 核心拿到收集器里的内容后，把它当成
//	"等用户输入"而不是失败，照常返回给上层。
//
// 这样既不需要真的挂起 goroutine，也不用改动 ReAct 循环本身。
package askuser

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
)

// ErrRequested 是 ask_user 工具用来中断本轮的哨兵错误。
//
// 它不会冒泡到用户可见的错误里：Agent 核心只要发现收集器里有内容，
// 就知道本轮是"等用户输入"，直接返回 Clarify 而不是错误。
var ErrRequested = errors.New("ask_user: clarification requested")

// 选项数量上下限：1 个选项等于没得选，太多会让用户无从下手。
const (
	MinOptions = 2
	MaxOptions = 6
)

// Option 澄清提问的一个可选项。
type Option struct {
	ID    string `json:"id"`
	Label string `json:"label"`          // 用户看到的文字
	Hint  string `json:"hint,omitempty"` // 可选的一句话说明
}

// Request 一次澄清请求。
type Request struct {
	Question string   `json:"question"`         // 向用户提出的问题
	Options  []Option `json:"options"`          // 可选项（2~6 个）
	Reason   string   `json:"reason,omitempty"` // Agent 为什么要问（便于排查与展示）
}

// Normalize 清洗模型给出的选项：去空、去重、截断到合法数量。
//
// 模型偶尔会返回空选项或者同一个 label 重复出现，直接透给前端会渲染出
// 一排点了没反应的按钮。这里统一收口，而不是指望提示词约束住。
func (r *Request) Normalize() {
	r.Question = strings.TrimSpace(r.Question)
	r.Reason = strings.TrimSpace(r.Reason)

	seen := make(map[string]bool, len(r.Options))
	cleaned := make([]Option, 0, len(r.Options))
	for i, o := range r.Options {
		label := strings.TrimSpace(o.Label)
		if label == "" {
			continue
		}
		key := strings.ToLower(label)
		if seen[key] {
			continue
		}
		seen[key] = true

		// 模型经常只给 label 不给 id，补一个稳定的序号作 id
		id := strings.TrimSpace(o.ID)
		if id == "" {
			id = "opt_" + strconv.Itoa(i+1)
		}
		cleaned = append(cleaned, Option{
			ID:    id,
			Label: label,
			Hint:  strings.TrimSpace(o.Hint),
		})
		if len(cleaned) >= MaxOptions {
			break
		}
	}
	r.Options = cleaned
}

// Valid 判断这请求是否值得发给用户。
func (r *Request) Valid() bool {
	return strings.TrimSpace(r.Question) != "" && len(r.Options) >= MinOptions
}

// AsText 把澄清请求渲染成一段纯文本，用于落库。
//
// 澄清那一轮助手其实"说了一句话"（就是这句提问）。把它存成助手发言，
// 历史就保持严格的 user/assistant 交替——否则下一轮带上补充信息后会变成
// 两条连续的 user 消息，部分上游会因此直接报错。
func (r *Request) AsText() string {
	var sb strings.Builder
	sb.WriteString(r.Question)
	if len(r.Options) > 0 {
		labels := make([]string, 0, len(r.Options))
		for _, o := range r.Options {
			labels = append(labels, o.Label)
		}
		sb.WriteString("\n可选：")
		sb.WriteString(strings.Join(labels, " / "))
	}
	return sb.String()
}

// ======================== 每轮收集器 ========================

// Collector 收集本轮发生的澄清请求。
//
// 生命周期与"一轮对话"严格一致：由服务层在开始一轮前创建、放进 ctx，
// 一轮结束后读取。Agent 核心不自己创建它——没有收集器就表示本轮
// 不需要澄清能力，这样非 Agent 模型完全不受影响。
type Collector struct {
	mu  sync.Mutex
	req *Request
}

func NewCollector() *Collector { return &Collector{} }

// Set 记录一次澄清请求（同一轮多次调用时保留第一个）。
//
// 保留第一个而不是最后一个：模型一旦决定问，后面再问只会让用户困惑。
func (c *Collector) Set(r Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.req != nil {
		return
	}
	r.Normalize()
	if !r.Valid() {
		return
	}
	c.req = &r
}

// Take 取出并清空。
func (c *Collector) Take() (Request, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.req == nil {
		return Request{}, false
	}
	r := *c.req
	c.req = nil
	return r, true
}

type collectorKey struct{}

// WithCollector 把收集器放进 ctx（由服务层在每轮开始时调用）。
func WithCollector(ctx context.Context, c *Collector) context.Context {
	if c == nil {
		return ctx
	}
	return context.WithValue(ctx, collectorKey{}, c)
}

// FromContext 取出本轮的收集器。
func FromContext(ctx context.Context) (*Collector, bool) {
	c, ok := ctx.Value(collectorKey{}).(*Collector)
	return c, ok && c != nil
}
