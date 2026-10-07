package tool

import (
	"context"
	"time"

	"deeptalk/internal/infra/metrics"
)

// defaultBackoffBase 指数退避基数：200ms → 400ms → 800ms …
const defaultBackoffBase = 200 * time.Millisecond

// maxBackoff 单次退避上限，避免重试等待本身拖垮整轮预算。
const maxBackoff = 5 * time.Second

// runWithRetry 按 ToolSpec 的重试策略执行一次工具调用。
//
// 只有幂等工具才会自动重试：对下单、扣款这类非幂等操作重试会造成重复副作用，
// 所以宁可把错误原样回填给模型，让模型自己决定下一步。
func runWithRetry(ctx context.Context, spec ToolSpec, argsJSON string) (string, error) {
	out, err := spec.Handler(ctx, argsJSON)
	if err == nil {
		return out, nil
	}
	if !spec.Idempotent {
		return out, err
	}

	attempts := spec.RetryPolicy.MaxAttempts
	if attempts <= 0 {
		attempts = defaultMaxAttempts
	}
	if attempts <= 1 {
		return out, err
	}

	base := spec.RetryPolicy.BackoffBase
	if base <= 0 {
		base = defaultBackoffBase
	}

	for i := 1; i < attempts; i++ {
		// ctx 已经结束（整轮超时 / 用户断开）就别再重试了
		if ctx.Err() != nil {
			return out, err
		}
		select {
		case <-ctx.Done():
			return out, err
		case <-time.After(backoff(base, i)):
		}

		next, nextErr := spec.Handler(ctx, argsJSON)
		if nextErr == nil {
			metrics.CountAgentToolRetry(spec.Name, true)
			return next, nil
		}
		out, err = next, nextErr
		metrics.CountAgentToolRetry(spec.Name, false)
	}
	return out, err
}

// backoff 指数退避并封顶。
func backoff(base time.Duration, attempt int) time.Duration {
	d := base << (attempt - 1)
	if d <= 0 || d > maxBackoff {
		return maxBackoff
	}
	return d
}
