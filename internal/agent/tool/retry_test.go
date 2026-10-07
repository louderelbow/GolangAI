package tool

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	einotool "github.com/cloudwego/eino/components/tool"
)

// TestNonIdempotentToolIsNotRetried 恒失败的非幂等工具只能调用一次。
// 对下单/扣款这类操作重试会造成重复副作用，所以宁可把错误交回模型。
//
// 注意"交回模型"不等于"作为 Go error 上抛"：工具失败会被回填成 observation
// （见 TestToolFailureBecomesObservation），否则 eino 的 ToolsNode 会中断
// 整个 graph，一个抖动的工具就让用户拿不到任何回答。
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
	out, err := inv.InvokableRun(context.Background(), "{}")
	if err != nil {
		t.Fatalf("工具失败应回填为 observation，不该上抛 error: %v", err)
	}
	if !strings.Contains(out, "non_idempotent") {
		t.Fatalf("observation 里应含工具名，便于模型判断是谁失败: %q", out)
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

// TestIdempotentToolGivesUpAfterMaxAttempts 用尽重试次数后回填 observation。
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
	out, err := inv.InvokableRun(context.Background(), "{}")
	if err != nil {
		t.Fatalf("用尽重试后应回填 observation，不该上抛 error: %v", err)
	}
	if !strings.Contains(out, "always_fails") {
		t.Fatalf("observation 里应含工具名: %q", out)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Fatalf("应按 MaxAttempts 尝试 3 次，实际 %d 次", got)
	}
}

// TestToolFailureBecomesObservation 工具失败必须回填成 observation 而不是上抛。
//
// 这是整轮不被打断的前提：eino 的 ToolsNode 遇到任一 task 错误会使整个 graph
// 失败，于是"一个 MCP 服务抖动"升级成"用户什么都拿不到"。
func TestToolFailureBecomesObservation(t *testing.T) {
	collector := NewWarnCollector()
	ctx := WithWarnings(context.Background(), collector)

	spec := ToolSpec{
		Name: "flaky",
		Handler: func(context.Context, string) (string, error) {
			return "", errors.New("upstream 503")
		},
	}

	inv := spec.BaseTool().(einotool.InvokableTool)
	out, err := inv.InvokableRun(ctx, "{}")
	if err != nil {
		t.Fatalf("工具失败不该中断整轮，实际 err=%v", err)
	}
	if !strings.Contains(out, "upstream 503") {
		t.Fatalf("observation 应带上原始错误，模型据此决定换参数还是如实告知: %q", out)
	}

	// 同时要留一条给用户的告警：否则用户看到的是一段照常输出的回答，
	// 无从分辨"没查到"和"查到了但没有"。
	warns := WarningsFrom(ctx)
	if len(warns) != 1 {
		t.Fatalf("应记 1 条告警，实际 %d 条: %+v", len(warns), warns)
	}
	if warns[0].Tool != "flaky" || warns[0].Message == "" {
		t.Fatalf("告警内容不对: %+v", warns[0])
	}
}

// TestInterruptOnErrorToolStillAborts ask_user 靠报错中断本轮，
// 不能被"回填成 observation"的新语义吞掉。
func TestInterruptOnErrorToolStillAborts(t *testing.T) {
	collector := NewWarnCollector()
	ctx := WithWarnings(context.Background(), collector)

	want := errors.New("ask_user: clarification requested")
	spec := ToolSpec{
		Name:             "ask_user",
		InterruptOnError: true,
		Handler: func(context.Context, string) (string, error) {
			return "", want
		},
	}

	inv := spec.BaseTool().(einotool.InvokableTool)
	if _, err := inv.InvokableRun(ctx, "{}"); !errors.Is(err, want) {
		t.Fatalf("中断型工具的错误必须原样上抛，实际 err=%v", err)
	}
	// 它不是故障，不该给用户弹"没取到数据"的提示
	if warns := WarningsFrom(ctx); len(warns) != 0 {
		t.Fatalf("中断型工具不该产生告警，实际 %+v", warns)
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
