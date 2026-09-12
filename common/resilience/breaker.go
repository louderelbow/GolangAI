// Package resilience 统一封装熔断能力。
//
// 用法：按"依赖"命名，一个依赖一个熔断器
//
// 熔断打开时立即返回 ErrOpen（下游根本不会被调用），调用方据此做降级。
package resilience

import (
	"context"
	"deeptalk/common/metrics"
	"deeptalk/config"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/sony/gobreaker/v2"
)

// ErrOpen 熔断打开（或 half-open 试探配额已满），本次调用被快速拒绝
var ErrOpen = errors.New("circuit breaker is open")

// ErrOpenFor 判断某个错误是否由熔断引起
func IsOpen(err error) bool { return errors.Is(err, ErrOpen) }

// 依赖命名约定（决定熔断粒度）：
//
//	llm:<model>     各模型独立——DeepSeek 挂了不该影响 DashScope
//	redis:<用途>    限流、配额分别熔断（配额挂了不该影响限流）
//	mcp:<tool>      每个工具独立，一个坏工具不拖垮其它工具
//	http:<service>  外部 HTTP（图片识别 / TTS / 天气）
const (
	PrefixLLM   = "llm:"
	PrefixRedis = "redis:"
	PrefixMCP   = "mcp:"
	PrefixHTTP  = "http:"
)

type registry struct {
	mu       sync.RWMutex
	breakers map[string]*gobreaker.CircuitBreaker[any]
}

var defaultRegistry = &registry{breakers: make(map[string]*gobreaker.CircuitBreaker[any])}

// breaker 取（或懒创建）某个依赖的熔断器
func (r *registry) breaker(name string) *gobreaker.CircuitBreaker[any] {
	r.mu.RLock()
	b := r.breakers[name]
	r.mu.RUnlock()
	if b != nil {
		return b
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if b = r.breakers[name]; b != nil {
		return b
	}

	cfg := config.GetConfig().GetResilience()
	b = gobreaker.NewCircuitBreaker[any](gobreaker.Settings{
		Name: name,
		// half-open 状态下允许同时放过的试探请求数
		MaxRequests: uint32(cfg.MaxRequestsHalfOpen),
		// 关闭状态下的计数窗口：到点清空历史计数，避免旧失败长期拖累失败率
		Interval: time.Duration(cfg.IntervalSeconds) * time.Second,
		// open 状态持续多久后进入 half-open 试探
		Timeout: time.Duration(cfg.TimeoutSeconds) * time.Second,

		ReadyToTrip: func(counts gobreaker.Counts) bool {
			// 规则一：连续失败达到阈值（快速响应突发故障）
			if counts.ConsecutiveFailures >= uint32(cfg.FailureThreshold) {
				return true
			}
			// 规则二：请求量足够且失败率超阈值（响应"一半请求失败"这类慢性故障）
			if counts.Requests >= uint32(cfg.MinRequests) {
				ratio := float64(counts.TotalFailures) / float64(counts.Requests)
				if ratio >= cfg.FailureRatio {
					return true
				}
			}
			return false
		},

		IsSuccessful: func(err error) bool {
			return err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
		},

		OnStateChange: func(name string, from, to gobreaker.State) {
			log.Printf("[breaker] %s: %s -> %s", name, from, to)
			metrics.SetCircuitState(name, to.String())
			metrics.CountCircuitEvent(name, to.String())
		},
	})

	r.breakers[name] = b
	return b
}

// States 返回全部熔断器状态（供 /metrics 或诊断接口使用）
func States() map[string]string {
	defaultRegistry.mu.RLock()
	defer defaultRegistry.mu.RUnlock()
	out := make(map[string]string, len(defaultRegistry.breakers))
	for name, b := range defaultRegistry.breakers {
		out[name] = b.State().String()
	}
	return out
}

// Do 在熔断器保护下执行 fn
// 熔断打开时立即返回 ErrOpen，fn 不会被执行
func Do[T any](name string, fn func() (T, error)) (T, error) {
	var zero T

	if config.GetConfig().GetResilience().Disabled {
		return fn()
	}

	res, err := defaultRegistry.breaker(name).Execute(func() (any, error) {
		return fn()
	})
	if err != nil {
		if errors.Is(err, gobreaker.ErrOpenState) || errors.Is(err, gobreaker.ErrTooManyRequests) {
			metrics.CountCircuitRejected(name)
			return zero, fmt.Errorf("%w (%s)", ErrOpen, name)
		}
		return zero, err
	}

	if res == nil {
		return zero, nil
	}
	v, ok := res.(T)
	if !ok {
		return zero, fmt.Errorf("breaker %s: unexpected result type %T", name, res)
	}
	return v, nil
}

// DoErr 无返回值的版本的熔断包装
func DoErr(name string, fn func() error) error {
	_, err := Do(name, func() (struct{}, error) {
		return struct{}{}, fn()
	})
	return err
}

// ModelKey 模型名 → 熔断器名
func ModelKey(modelName string) string {
	return PrefixLLM + strings.TrimSpace(modelName)
}

// MCPKey 工具名 → 熔断器名
func MCPKey(toolName string) string { return PrefixMCP + toolName }

// HTTPKey 外部服务名 → 熔断器名
func HTTPKey(service string) string { return PrefixHTTP + service }

// RedisKey Redis 用途 → 熔断器名
func RedisKey(usage string) string { return PrefixRedis + usage }
