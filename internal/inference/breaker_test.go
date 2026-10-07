package inference

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"deeptalk/internal/infra/config"
)

func testBreakerConfig() config.InferenceBreakerConfig {
	return config.InferenceBreakerConfig{
		FailureThreshold: 5,
		OpenDurationMs:   10000,
		HalfOpenProbes:   3,
	}
}

// 「连续真实失败会打开熔断」的正向用例在 scheduler_test.go 里
// （TestBreakerOpensAfterConsecutiveFailures）—— 那条走的是完整的
// Scheduler.Wrap → Generate 路径，比在这里直接调 Record 更有价值。
//
// 本文件只补它没覆盖的那一半：**什么不该算失败**。

// TestBreakerIgnoresCallerCancellation 用户主动取消**不能**打开熔断。
//
// 这条防的是一个很荒唐的假阳性：用户点「停止生成」或关掉页面 → ctx 取消 →
// context.Canceled 被算成下游失败 → **5 个用户退页面就把模型熔断掉**，
// 而下游其实完全健康。接下来 10 秒所有请求被零延迟拒绝。
//
// 这种 bug 特别隐蔽：生产上表现为"偶发性的服务不可用"，而且一查下游指标
// 全绿 —— 因为故障根本不在下游，在这一行判定里。
func TestBreakerIgnoresCallerCancellation(t *testing.T) {
	b := newBreaker(testBreakerConfig())

	// 5 次取消，正好等于 failureThreshold
	for i := 1; i <= 5; i++ {
		b.Record("m", context.Canceled)
	}

	if err := b.Allow("m"); err != nil {
		t.Fatalf("调用方取消不该打开熔断，实际 %v", err)
	}
	if got := b.State("m"); got != StateClosed {
		t.Fatalf("状态应当仍是 closed，实际 %s", got)
	}
}

// TestBreakerWrappedCancellationAlsoIgnored 包装过的取消也要认出来。
//
// 真实链路里 ctx 取消往往不是裸的 context.Canceled ——
// http.Client、eino、各家 SDK 都会再包一层。所以判定必须用 errors.Is，
// 而不是 err == context.Canceled。
func TestBreakerWrappedCancellationAlsoIgnored(t *testing.T) {
	b := newBreaker(testBreakerConfig())

	wrapped := fmt.Errorf("模型调用失败: %w", context.Canceled)
	for i := 1; i <= 5; i++ {
		b.Record("m", wrapped)
	}

	if err := b.Allow("m"); err != nil {
		t.Fatalf("包装过的取消同样不该打开熔断，实际 %v", err)
	}
}

// TestBreakerTimeoutIsNotExempt 超时必须算失败 —— 这一点和取消相反。
//
// 下游超时（连接超时 / 读超时）恰恰是最常见的故障形态，把它也豁免掉的话，
// 熔断器就永远不会跳闸了。（resilience 那套的注释里专门讲过这个坑：
// go-redis 之类的客户端在连接超时后返回的错误正好满足
// errors.Is(err, context.DeadlineExceeded)。）
func TestBreakerTimeoutIsNotExempt(t *testing.T) {
	b := newBreaker(testBreakerConfig())

	for i := 1; i <= 5; i++ {
		b.Record("m", context.DeadlineExceeded)
	}

	if err := b.Allow("m"); !errors.Is(err, ErrBreakerOpen) {
		t.Fatalf("连续超时必须打开熔断，实际 %v", err)
	}
}

// TestBreakerIsPerModel 熔断按模型隔离：一个模型坏了不牵连另一个。
func TestBreakerIsPerModel(t *testing.T) {
	b := newBreaker(testBreakerConfig())

	for i := 0; i <= 5; i++ {
		b.Record("bad-model", errors.New("上游 500"))
	}

	if err := b.Allow("bad-model"); !errors.Is(err, ErrBreakerOpen) {
		t.Fatalf("坏模型应当被熔断，实际 %v", err)
	}
	if err := b.Allow("good-model"); err != nil {
		t.Fatalf("另一个模型不该受影响，实际 %v", err)
	}
}

// TestBreakerNilSafe 没启用调度时守卫为 nil，调用点不必判空。
func TestBreakerNilSafe(t *testing.T) {
	var b *Breaker
	if err := b.Allow("m"); err != nil {
		t.Errorf("nil breaker 不该拒绝: %v", err)
	}
	b.Record("m", errors.New("x")) // 不应 panic
	if got := b.State("m"); got != StateClosed {
		t.Errorf("nil breaker 的状态应当是 closed，实际 %s", got)
	}
}
