package tool

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	einotool "github.com/cloudwego/eino/components/tool"
)

// TestNonIdempotentToolIsNotRetried 恒失败的非幂等工具只能调用一次。
// 对下单/扣款这类操作重试会造成重复副作用，所以宁可把错误交回模型。
func TestNonIdempotentToolIsNotRetried(t *testing.T) {
	var calls int32
	spec := ToolSpec{
		Name:       "non_idempotent",
		Idempotent: false,
		RetryPolicy: RetryPolicy{
			MaxAttempts: 3,
			BackoffBase: time.Millisecond,
		},
		Handler: func(context.Context, string) (string, error) {
			atomic.AddInt32(&calls, 1)
			return "", errors.New("always fails")
		},
	}

	inv := spec.BaseTool().(einotool.InvokableTool)
	if _, err := inv.InvokableRun(context.Background(), "{}"); err == nil {
		t.Fatal("恒失败的工具应返回错误")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("非幂等工具不应重试，实际调用 %d 次", got)
	}
}

// TestIdempotentToolRetriesOnce 幂等工具前一次失败、第二次成功：
// 应重试一次并返回成功结果。
func TestIdempotentToolRetriesOnce(t *testing.T) {
	var calls int32
	spec := ToolSpec{
		Name:       "idempotent",
		Idempotent: true,
		RetryPolicy: RetryPolicy{
			MaxAttempts: 2,
			BackoffBase: time.Millisecond,
		},
		Handler: func(context.Context, string) (string, error) {
			if atomic.AddInt32(&calls, 1) == 1 {
				return "", errors.New("transient failure")
			}
			return "ok-after-retry", nil
		},
	}

	inv := spec.BaseTool().(einotool.InvokableTool)
	out, err := inv.InvokableRun(context.Background(), "{}")
	if err != nil {
		t.Fatalf("重试后应成功，实际报错: %v", err)
	}
	if out != "ok-after-retry" {
		t.Fatalf("返回内容不对: %q", out)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("应重试 1 次（共 2 次调用），实际 %d 次", got)
	}
}

// TestIdempotentToolGivesUpAfterMaxAttempts 用尽重试次数后仍应返回错误。
func TestIdempotentToolGivesUpAfterMaxAttempts(t *testing.T) {
	var calls int32
	spec := ToolSpec{
		Name:       "always_fails",
		Idempotent: true,
		RetryPolicy: RetryPolicy{
			MaxAttempts: 3,
			BackoffBase: time.Millisecond,
		},
		Handler: func(context.Context, string) (string, error) {
			atomic.AddInt32(&calls, 1)
			return "", errors.New("always fails")
		},
	}

	inv := spec.BaseTool().(einotool.InvokableTool)
	if _, err := inv.InvokableRun(context.Background(), "{}"); err == nil {
		t.Fatal("用尽重试后应返回错误")
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Fatalf("应按 MaxAttempts 尝试 3 次，实际 %d 次", got)
	}
}

// TestRetryStopsWhenContextCancelled 上下文已结束时不应再重试：
// 整轮已经超时/用户已断开，继续重试只是白烧上游调用。
func TestRetryStopsWhenContextCancelled(t *testing.T) {
	var calls int32
	spec := ToolSpec{
		Name:       "cancelled",
		Idempotent: true,
		Timeout:    time.Second,
		RetryPolicy: RetryPolicy{
			MaxAttempts: 5,
			BackoffBase: time.Millisecond,
		},
		Handler: func(ctx context.Context, _ string) (string, error) {
			atomic.AddInt32(&calls, 1)
			return "", errors.New("fails")
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	inv := spec.BaseTool().(einotool.InvokableTool)
	if _, err := inv.InvokableRun(ctx, "{}"); err == nil {
		t.Fatal("ctx 已取消时应返回错误")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("ctx 取消后不应重试，实际调用 %d 次", got)
	}
}

// TestBackoffGrowsAndCaps 指数退避要增长且封顶。
func TestBackoffGrowsAndCaps(t *testing.T) {
	base := 100 * time.Millisecond
	first := backoff(base, 1)
	second := backoff(base, 2)
	third := backoff(base, 3)

	if first != base {
		t.Fatalf("首次退避应为 %s，实际 %s", base, first)
	}
	if second != 2*base {
		t.Fatalf("第二次退避应为 %s，实际 %s", 2*base, second)
	}
	if third != 4*base {
		t.Fatalf("第三次退避应为 %s，实际 %s", 4*base, third)
	}
	// 足够大的 attempt 必须被封顶，否则重试等待本身会拖垮整轮预算
	if capped := backoff(base, 20); capped > maxBackoff {
		t.Fatalf("退避应封顶在 %s，实际 %s", maxBackoff, capped)
	}
}
