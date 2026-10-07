// Package inference 提供推理请求的调度：排队、并发槽位、负载均衡与准入熔断。
//
// 为什么放在"模型"这一层而不是 Agent 层：并发上限是**每个上游模型**的属性
// （不同模型的配额与限流不同），而模型层是所有推理调用的必经之路——
// 无论上层是纯对话、RAG 还是 Agent，最终都会落到这里。
//
// 调度顺序（顺序本身就是设计）：
//
//	准入熔断 → 取并发槽位（满了就排队）→ 负载均衡选实例 → 调用 → 记录延迟与结果
//
// 熔断放在最前面，是为了让已知故障的模型在**入队之前**就被拒掉：
// 排队要占名额、还要等 queueTimeout，对注定失败的请求纯属浪费。
package inference

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"deeptalk/internal/infra/config"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// 调度器返回的哨兵错误。上层据此返回明确的业务错误码，而不是 500。
var (
	ErrQueueFull    = errors.New("inference queue full")
	ErrQueueTimeout = errors.New("inference queue timeout")
	ErrBreakerOpen  = errors.New("inference breaker open")
	ErrShuttingDown = errors.New("inference scheduler is shutting down")
	errNoModel      = errors.New("inference: model is required")
)

// Scheduler 按模型维护推理池与准入熔断。
type Scheduler struct {
	enabled bool

	queueTimeout  time.Duration
	maxQueueDepth int

	breaker *Breaker

	mu     sync.Mutex
	pools  map[string]*Pool
	closed bool
}

// NewScheduler 按配置构造调度器；enabled=false 时返回一个直通实例。
func NewScheduler(cfg config.InferenceConfig) *Scheduler {
	if !cfg.Enabled {
		return &Scheduler{enabled: false}
	}

	s := &Scheduler{
		enabled:       true,
		queueTimeout:  time.Duration(cfg.QueueTimeoutMs) * time.Millisecond,
		maxQueueDepth: cfg.MaxQueueDepth,
		breaker:       newBreaker(cfg.Breaker),
		pools:         make(map[string]*Pool, len(cfg.Pools)),
	}

	models := make([]string, 0, len(cfg.Pools))
	for name, pc := range cfg.Pools {
		models = append(models, name)
		s.pools[name] = newPool(name, pc, s.queueTimeout, s.maxQueueDepth)
	}
	// 分桶必须在任何观测之前注册，否则会固化成默认的秒级分桶
	registerMetrics(models)
	return s
}

// 进程级共享调度器。
//
// 必须是全局的：并发上限是**上游模型**的属性，不是会话的属性。
// 每个会话建一个调度器，等于每个会话各自有 maxConcurrent 个槽位，
// 上限就形同虚设——这是"看起来做了限流、实际没有"的典型陷阱。
var (
	sharedOnce sync.Once
	shared     *Scheduler
)

// Shared 返回进程级共享调度器（首次调用时按配置初始化）。
func Shared() *Scheduler {
	sharedOnce.Do(func() {
		shared = NewScheduler(config.GetConfig().GetInference())
	})
	return shared
}

// Enabled 是否真正在做调度；false 时 Wrap 返回原模型。
func (s *Scheduler) Enabled() bool { return s != nil && s.enabled }

// poolFor 取某个模型的池；未在配置里出现的模型按默认参数现场建一个。
//
// 现场建而不是报错：新增模型时忘了配 [inference.pools.xxx] 更常见，
// 这时给一个保守上限（默认 10 并发）比"完全没有限制"安全。
func (s *Scheduler) poolFor(name string) *Pool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.pools[name]; ok {
		return p
	}
	p := newPool(name, config.InferencePoolConfig{}, s.queueTimeout, s.maxQueueDepth)
	s.pools[name] = p
	return p
}

// acquire 走完"熔断准入 + 取槽位"两步，成功后返回归还函数。
func (s *Scheduler) acquire(ctx context.Context, name string) (func(), error) {
	if !s.Enabled() {
		return func() {}, nil
	}

	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		countRejected(name, ReasonShuttingDown)
		return nil, ErrShuttingDown
	}

	pool := s.poolFor(name)
	if err := pool.Acquire(ctx); err != nil {
		return nil, err
	}
	return pool.Release, nil
}

// admit 走完"熔断准入 → 取槽位"，成功后返回归还函数。
//
// 顺序不能颠倒：熔断放在排队之前，已知故障的模型就不会白占队列名额，
// 也不必等满 queueTimeout 才失败。
func (s *Scheduler) admit(ctx context.Context, name string) (func(), error) {
	if err := s.breaker.Allow(name); err != nil {
		return nil, err
	}
	return s.acquire(ctx, name)
}

// Wrap 把调度能力套在一个模型上。
//
// 返回的对象仍然满足 model.ToolCallingChatModel，因此可以和 guard 的
// 预算/兜底包装任意组合，顺序不同语义略有差异（预算在外层则预算更宽松）。
func (s *Scheduler) Wrap(modelName string, inner model.ToolCallingChatModel) model.ToolCallingChatModel {
	if !s.Enabled() || inner == nil {
		return inner
	}
	return &wrapped{s: s, model: modelName, inner: inner}
}

// wrapped 是调度器对 model.ToolCallingChatModel 的适配层。
type wrapped struct {
	s     *Scheduler
	model string
	inner model.ToolCallingChatModel
}

func (w *wrapped) WithTools(tools []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	next, err := w.inner.WithTools(tools)
	if err != nil {
		return nil, err
	}
	return &wrapped{s: w.s, model: w.model, inner: next}, nil
}

func (w *wrapped) Generate(ctx context.Context, in []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	release, err := w.s.admit(ctx, w.model)
	if err != nil {
		return nil, err
	}
	defer release()

	start := time.Now()
	msg, err := w.inner.Generate(ctx, in, opts...)
	observeLatency(w.model, time.Since(start))
	// 结果必须回送给准入熔断器，否则它永远看不到上游失败、也就永远不会打开
	w.s.breaker.Record(w.model, err)
	return msg, err
}

// Stream 的槽位要一直占到流读完为止。
//
// eino 的 StreamReader 只在收流层被 Close，调度器拿不到结束回调，
// 因此这里自己 pump 一遍：转发结束（读尽 / 报错 / 下游关闭 / ctx 取消）
// 就是"这一路推理结束"，此刻才归还槽位。
// 若只在建流成功后立刻归还，并发上限对流式请求就形同虚设——
// 而流式恰恰是本项目最主要的调用形态。
func (w *wrapped) Stream(ctx context.Context, in []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	release, err := w.s.admit(ctx, w.model)
	if err != nil {
		return nil, err
	}

	start := time.Now()
	src, err := w.inner.Stream(ctx, in, opts...)
	observeLatency(w.model, time.Since(start))
	if err != nil {
		w.s.breaker.Record(w.model, err)
		release()
		return nil, err
	}

	// 建流成功只说明"握上手了"，中途报错同样要记进熔断器，
	// 所以把记录动作交给 pump 在流真正结束时执行。
	return pump(ctx, src, release, func(streamErr error) {
		w.s.breaker.Record(w.model, streamErr)
	}), nil
}

// pump 把源流转发到一个新流，并在结束时归还槽位与上报结果。
func pump(
	ctx context.Context,
	src *schema.StreamReader[*schema.Message],
	release func(),
	onDone func(error),
) *schema.StreamReader[*schema.Message] {
	out, sw := schema.Pipe[*schema.Message](8)

	go func() {
		var finalErr error
		defer func() {
			onDone(finalErr)
			release()
			sw.Close()
			src.Close()
		}()

		for {
			select {
			case <-ctx.Done():
				finalErr = ctx.Err()
				sw.Send(nil, finalErr)
				return
			default:
			}

			msg, err := src.Recv()
			if err != nil {
				if !errors.Is(err, io.EOF) {
					finalErr = err
					sw.Send(nil, err)
				}
				return
			}
			if sw.Send(msg, nil) {
				// 下游已关闭（客户端断开）：停止转发并归还槽位。
				// 这不算上游失败，因此不记入熔断器。
				return
			}
		}
	}()

	return out
}

// ==================== 生命周期 ====================

// Close 停止接收新请求：新请求立刻被拒，排队中的请求被唤醒并拿到错误。
// 已在执行的请求不受影响。
func (s *Scheduler) Close() {
	if !s.Enabled() {
		return
	}
	s.mu.Lock()
	s.closed = true
	pools := make([]*Pool, 0, len(s.pools))
	for _, p := range s.pools {
		pools = append(pools, p)
	}
	s.mu.Unlock()

	for _, p := range pools {
		p.Close()
	}
}

// Drain 等待所有在途推理结束，最多等 d；返回是否已排空。
func (s *Scheduler) Drain(d time.Duration) bool {
	if !s.Enabled() {
		return true
	}
	s.mu.Lock()
	pools := make([]*Pool, 0, len(s.pools))
	for _, p := range s.pools {
		pools = append(pools, p)
	}
	s.mu.Unlock()

	deadline := time.Now().Add(d)
	ok := true
	for _, p := range pools {
		remain := time.Until(deadline)
		if remain <= 0 {
			return false
		}
		if !p.Drain(remain) {
			ok = false
		}
	}
	return ok
}

// Stats 返回某模型的（在途, 排队）数量，供测试与排查使用。
func (s *Scheduler) Stats(modelName string) (inflight, queued int) {
	if !s.Enabled() {
		return 0, 0
	}
	return s.poolFor(modelName).Stats()
}

// BreakerState 返回某模型的准入熔断状态。
func (s *Scheduler) BreakerState(modelName string) string {
	if !s.Enabled() {
		return StateClosed
	}
	return s.breaker.State(modelName)
}
