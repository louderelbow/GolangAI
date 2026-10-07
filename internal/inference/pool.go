package inference

import (
	"context"
	"sync"
	"time"

	"deeptalk/internal/infra/config"
)

// Pool 一个模型的推理池：maxConcurrent 个并发槽位 + 有界优先级队列。
//
// 语义：
//   - 有空槽位 → 直接进入（不排队）
//   - 槽位用满   → 按优先级排队，等待别人释放
//   - 队列满     → 立即拒绝（快速失败，不让请求白等）
//   - 排队超时   → 拒绝并把自己从队列摘掉
type Pool struct {
	name          string
	maxConcurrent int
	weight        int

	queueTimeout  time.Duration
	maxQueueDepth int

	mu       sync.Mutex
	inflight int
	waiters  priorityQueue
	closed   bool
}

func newPool(name string, cfg config.InferencePoolConfig, queueTimeout time.Duration, maxQueueDepth int) *Pool {
	maxConcurrent := cfg.MaxConcurrent
	if maxConcurrent <= 0 {
		// 没配置也要有个上限：无限并发等于没有保护
		maxConcurrent = 10
	}
	weight := cfg.Weight
	if weight <= 0 {
		weight = 1
	}
	return &Pool{
		name:          name,
		maxConcurrent: maxConcurrent,
		weight:        weight,
		queueTimeout:  queueTimeout,
		maxQueueDepth: maxQueueDepth,
	}
}

// Acquire 申请一个槽位。返回 nil 表示拿到槽位，调用方必须在结束后 Release。
func (p *Pool) Acquire(ctx context.Context) error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		countRejected(p.name, ReasonShuttingDown)
		return ErrShuttingDown
	}

	// 快路径：有槽位且没人在等，直接进（保证不插队）
	if p.inflight < p.maxConcurrent && p.waiters.Len() == 0 {
		p.inflight++
		inflight := p.inflight
		p.mu.Unlock()
		setInflight(p.name, inflight)
		return nil
	}

	// 需要排队：先看队列是否已满
	if p.waiters.Len() >= p.maxQueueDepth {
		p.mu.Unlock()
		countRejected(p.name, ReasonQueueFull)
		return ErrQueueFull
	}

	w := newWaiter(PriorityFrom(ctx))
	p.waiters.Push(w)
	queued := p.waiters.Len()
	p.mu.Unlock()
	setQueueDepth(p.name, queued)

	timer := time.NewTimer(p.queueTimeout)
	defer timer.Stop()

	select {
	case <-w.ready:
		// 被 Release 转交（拿到槽位）或被 Close 唤醒（没拿到）
		if w.err != nil {
			return w.err
		}
		observeQueueWait(p.name, time.Since(w.enqueued))
		return nil

	case <-timer.C:
		if p.dropWaiter(w) {
			countRejected(p.name, ReasonQueueTimeout)
			return ErrQueueTimeout
		}
		// 摘除失败 = Release 已经选中我：槽位归我所有，必须接受
		<-w.ready
		return w.err

	case <-ctx.Done():
		if p.dropWaiter(w) {
			return ctx.Err()
		}
		<-w.ready
		return w.err
	}
}

// dropWaiter 把自己从队列里摘掉，并同步队列深度指标。
// 返回 false 表示已经被 Release 选中（此时不能再放弃槽位，否则会漏计 inflight）。
func (p *Pool) dropWaiter(w *waiter) bool {
	p.mu.Lock()
	removed := p.waiters.Remove(w)
	queued := p.waiters.Len()
	p.mu.Unlock()
	if removed {
		setQueueDepth(p.name, queued)
	}
	return removed
}

// Release 归还槽位：优先直接转交给排队中的最高优先级请求，避免重新排队。
func (p *Pool) Release() {
	p.mu.Lock()
	if next := p.waiters.Pop(); next != nil {
		queued := p.waiters.Len()
		p.mu.Unlock()
		setQueueDepth(p.name, queued)
		next.err = nil
		next.ready <- struct{}{} // 缓冲通道，不会阻塞
		return
	}
	if p.inflight > 0 {
		p.inflight--
	}
	inflight := p.inflight
	p.mu.Unlock()
	setInflight(p.name, inflight)
}

// Close 停止接收新请求，并唤醒所有仍在排队的请求（它们会拿到 ErrShuttingDown）。
// 已经在执行的请求不受影响，由 Drain 等待它们结束。
func (p *Pool) Close() {
	p.mu.Lock()
	p.closed = true
	pending := p.waiters.drain()
	p.mu.Unlock()

	setQueueDepth(p.name, 0)
	for _, w := range pending {
		countRejected(p.name, ReasonShuttingDown)
		w.err = ErrShuttingDown
		w.ready <- struct{}{}
	}
}

// Drain 等待在途请求全部结束，最多等 d；返回是否已排空。
func (p *Pool) Drain(d time.Duration) bool {
	deadline := time.Now().Add(d)
	for {
		p.mu.Lock()
		idle := p.inflight == 0
		p.mu.Unlock()
		if idle {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Stats 返回当前在途数与排队数，供测试与日志使用。
func (p *Pool) Stats() (inflight, queued int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.inflight, p.waiters.Len()
}

func (p *Pool) Name() string { return p.name }

func (p *Pool) MaxConcurrent() int { return p.maxConcurrent }

func (p *Pool) Weight() int { return p.weight }
