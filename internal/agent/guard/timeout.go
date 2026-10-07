// Package guard 为 Agent 循环提供超时、预算与兜底三类可靠性保障。
//
// 设计取舍：预算不能靠"包一层 Agent"实现——ReAct 循环内部每一步的 token
// 消耗对外不可见。因此预算通过装饰**模型**来施加（见 budget.go），
// 超时通过 context 传递（见 timeout.go），模型级兜底通过替换模型实现
// （见 fallback.go）。这样既不改动循环实现，又能真正卡住用量。
package guard

import (
	"context"
	"errors"
	"time"
)

var (
	// ErrTurnTimeout 整轮超时：调用方应返回已产出的部分内容，而不是空响应
	ErrTurnTimeout = errors.New("agent turn timeout")
	// ErrBudgetExceeded 预算耗尽（步数 / token / 墙钟）
	ErrBudgetExceeded = errors.New("agent budget exceeded")
	// ErrAllModelsFailed 主模型与全部备用模型都失败
	ErrAllModelsFailed = errors.New("all models failed")
)

// Limits 是一次 Agent 执行的资源上限。零值表示该项不限制。
type Limits struct {
	MaxSteps     int           // 单轮最大模型调用次数
	MaxTokens    int           // 单轮累计 token 上限
	MaxWallClock time.Duration // 单轮墙钟上限
	ModelTimeout time.Duration // 单次模型调用超时
}

// WithTurnTimeout 给整轮执行套墙钟上限；d<=0 表示不限制。
func WithTurnTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if d <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, d)
}

// WithModelTimeout 给单次模型调用套超时；d<=0 表示不限制。
func WithModelTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if d <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, d)
}

// TurnTimedOut 判断错误是否由整轮超时引起。
func TurnTimedOut(err error) bool {
	return errors.Is(err, ErrTurnTimeout) || errors.Is(err, context.DeadlineExceeded)
}
