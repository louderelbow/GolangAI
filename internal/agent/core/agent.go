// Package core 定义 Agent 的稳定执行接口与 ReAct 骨架。
//
// 设计意图：把"Agent 怎么跑"和"用哪个模型"分开。
// guard（超时 / 预算 / 兜底）、trace（轨迹记录）都实现同一个 Agent 接口，
// 以装饰器方式叠加，模型接入层不需要感知这些能力的存在。
package core

import (
	"context"

	"github.com/cloudwego/eino/schema"
)

// StreamCallback 接收流式文本增量。
type StreamCallback func(chunk string)

// Request 一次 Agent 执行的输入。
type Request struct {
	SessionID string
	UserName  string
	Messages  []*schema.Message
}

// Response 一次 Agent 执行的输出。
//
// 只保留当前确实能填充的字段：不能验证的字段一律不加，
// 否则会出现"接口上有、实际永远是零值"的假象。
//
// 关于"本轮是回答还是反问"：那是**轮次级**的结论，由服务层从本轮的
// askuser 收集器里读。这里刻意不掺和——收集器只能被消费一次，
// 核心层也读一遍的话，服务层就拿不到东西了（这个坑踩过）。
type Response struct {
	Content string
	Usage   *schema.TokenUsage
}

// Agent 是可被装饰的最小执行单元。
type Agent interface {
	// Run 一次性返回完整回答
	Run(ctx context.Context, req Request) (*Response, error)
	// Stream 边生成边回调，返回完整回答与用量
	Stream(ctx context.Context, req Request, cb StreamCallback) (*Response, error)
}
