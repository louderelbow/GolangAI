package inference

import (
	"sync"
	"time"

	"deeptalk/internal/infra/config"

	"github.com/sony/gobreaker/v2"
)

// Breaker 调度层的**准入**熔断：在入队之前快速拒绝。
//
//	resilience（[resilience]）    按失败率熔断，作用在「调用过程中」，
//	                              目的是不要继续打已经坏掉的上游
//	Breaker（[inference.breaker]）
//	                              按「连续失败次数」在「入队前」短路，
//	                              目的是别把已知故障模型的队列排满 ——
//	                              排队要占名额、还要等 queueTimeout，
//	                              对注定失败的请求纯属浪费
type Breaker struct {
	cfg config.InferenceBreakerConfig

	mu       sync.Mutex
	breakers map[string]*gobreaker.CircuitBreaker[any]
}

func newBreaker(cfg config.InferenceBreakerConfig) *Breaker {
	return &Breaker{cfg: cfg, breakers: map[string]*gobreaker.CircuitBreaker[any]{}}
}

func (b *Breaker) breakerFor(model string) *gobreaker.CircuitBreaker[any] {
	b.mu.Lock()
	defer b.mu.Unlock()
	if cb, ok := b.breakers[model]; ok {
		return cb
	}

	failureThreshold := b.cfg.FailureThreshold
	halfOpenProbes := uint32(b.cfg.HalfOpenProbes)

	cb := gobreaker.NewCircuitBreaker[any](gobreaker.Settings{
		Name: model,
		// 半开状态下允许几次探测通过
		MaxRequests: halfOpenProbes,
		// 不做周期性清零：只在状态迁移时重置计数，
		// 这样"连续失败"的语义才是真的连续
		Interval: 0,
		Timeout:  time.Duration(b.cfg.OpenDurationMs) * time.Millisecond,
		ReadyToTrip: func(c gobreaker.Counts) bool {
			return int(c.ConsecutiveFailures) >= failureThreshold
		},
		IsSuccessful: func(err error) bool { return err == nil },
		OnStateChange: func(name string, _, to gobreaker.State) {
			setBreakerState(name, stateName(to))
		},
	})

	b.breakers[model] = cb
	setBreakerState(model, stateName(cb.State()))
	return cb
}

// Allow 是准入检查：熔断打开时立刻拒绝。
func (b *Breaker) Allow(model string) error {
	if b == nil {
		return nil
	}
	if b.breakerFor(model).State() == gobreaker.StateOpen {
		countRejected(model, ReasonBreakerOpen)
		return ErrBreakerOpen
	}
	return nil
}

// Record 在调用结束后把结果交给状态机。
func (b *Breaker) Record(model string, err error) {
	if b == nil {
		return
	}
	_, _ = b.breakerFor(model).Execute(func() (any, error) { return nil, err })
}

// State 返回某模型当前的熔断状态，供测试与排查使用。
func (b *Breaker) State(model string) string {
	if b == nil {
		return StateClosed
	}
	return stateName(b.breakerFor(model).State())
}

func stateName(s gobreaker.State) string {
	switch s {
	case gobreaker.StateOpen:
		return StateOpen
	case gobreaker.StateHalfOpen:
		return StateHalfOpen
	default:
		return StateClosed
	}
}
