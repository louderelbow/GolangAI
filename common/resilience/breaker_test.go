package resilience

import (
	"context"
	"errors"
	"testing"
	"time"

	"deeptalk/config"
)

// 连续失败达到阈值 → 熔断打开 → 后续调用被快速拒绝（fn 不再执行）
func TestBreakerOpensAfterConsecutiveFailures(t *testing.T) {
	cfg := config.GetConfig()
	// 用极端配置让测试确定：连续 3 次失败即跳闸
	cfg.ResilienceConfig.Disabled = false
	cfg.ResilienceConfig.FailureThreshold = 3
	cfg.ResilienceConfig.FailureRatio = 1
	cfg.ResilienceConfig.MinRequests = 1000
	cfg.ResilienceConfig.TimeoutSeconds = 1
	cfg.ResilienceConfig.MaxRequestsHalfOpen = 1
	t.Cleanup(func() { cfg.ResilienceConfig = config.ResilienceConfig{} })

	name := "test:open-after-failures"
	boom := errors.New("upstream down")

	calls := 0
	fail := func() (int, error) {
		calls++
		return 0, boom
	}

	// 前 3 次真实调用并失败
	for i := 0; i < 3; i++ {
		if _, err := Do(name, fail); !errors.Is(err, boom) {
			t.Fatalf("第 %d 次应返回原始错误，实际 %v", i+1, err)
		}
	}

	// 第 4 次应被熔断拦住，fn 不再执行
	if _, err := Do(name, fail); !IsOpen(err) {
		t.Fatalf("熔断应已打开，实际 err=%v", err)
	}
	if calls != 3 {
		t.Fatalf("熔断打开后不应再调用下游，实际调用次数=%d", calls)
	}

	if got := States()[name]; got != "open" {
		t.Fatalf("状态应为 open，实际 %s", got)
	}
}

// 熔断打开后，等 Timeout 进入 half-open，试探成功则恢复 closed
func TestBreakerRecoversAfterTimeout(t *testing.T) {
	cfg := config.GetConfig()
	cfg.ResilienceConfig.Disabled = false
	cfg.ResilienceConfig.FailureThreshold = 2
	cfg.ResilienceConfig.FailureRatio = 1
	cfg.ResilienceConfig.MinRequests = 1000
	cfg.ResilienceConfig.TimeoutSeconds = 1 // 1 秒后允许试探
	cfg.ResilienceConfig.MaxRequestsHalfOpen = 1
	t.Cleanup(func() { cfg.ResilienceConfig = config.ResilienceConfig{} })

	name := "test:recovery"
	fail := func() (string, error) { return "", errors.New("down") }

	for i := 0; i < 2; i++ {
		_, _ = Do(name, fail)
	}
	if _, err := Do(name, fail); !IsOpen(err) {
		t.Fatalf("应已熔断，实际 %v", err)
	}

	// 等过 Timeout，进入 half-open
	time.Sleep(1200 * time.Millisecond)

	ok := func() (string, error) { return "healthy", nil }
	got, err := Do(name, ok)
	if err != nil {
		t.Fatalf("half-open 试探应成功，实际 %v", err)
	}
	if got != "healthy" {
		t.Fatalf("结果异常: %q", got)
	}
	if states := States()[name]; states != "closed" {
		t.Fatalf("试探成功后应回到 closed，实际 %s", states)
	}
}

// Disabled=true 时直连，不做任何熔断
func TestBreakerDisabled(t *testing.T) {
	cfg := config.GetConfig()
	cfg.ResilienceConfig.Disabled = true
	t.Cleanup(func() { cfg.ResilienceConfig = config.ResilienceConfig{} })

	name := "test:disabled"
	calls := 0
	fail := func() (int, error) { calls++; return 0, errors.New("down") }

	for i := 0; i < 10; i++ {
		_, _ = Do(name, fail)
	}
	if calls != 10 {
		t.Fatalf("禁用熔断时应每次都调用下游，实际 %d 次", calls)
	}
}

// 上下文取消不算下游故障（用户关页面不该把服务判死）
func TestCanceledContextNotCountedAsFailure(t *testing.T) {
	cfg := config.GetConfig()
	cfg.ResilienceConfig.Disabled = false
	cfg.ResilienceConfig.FailureThreshold = 2
	cfg.ResilienceConfig.TimeoutSeconds = 30
	t.Cleanup(func() { cfg.ResilienceConfig = config.ResilienceConfig{} })

	name := "test:canceled"
	canceled := func() (int, error) { return 0, context.Canceled }

	for i := 0; i < 5; i++ {
		_, _ = Do(name, canceled)
	}
	if s := States()[name]; s == "open" {
		t.Fatal("context.Canceled 不应触发熔断")
	}
}
