package inference

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"deeptalk/internal/infra/config"
)

// ==================== 优先级队列 ====================

func TestQueuePreemptsByPriorityAndKeepsFIFOWithinLevel(t *testing.T) {
	q := &priorityQueue{}

	low1 := newWaiter(PriorityLow)
	low2 := newWaiter(PriorityLow)
	normal := newWaiter(PriorityNormal)
	high1 := newWaiter(PriorityHigh)
	high2 := newWaiter(PriorityHigh)

	// 故意按"最差情况"入队，验证出队顺序只由优先级与入队顺序决定
	q.Push(low1)
	q.Push(normal)
	q.Push(high1)
	q.Push(low2)
	q.Push(high2)

	want := []*waiter{high1, high2, normal, low1, low2}
	for i, w := range want {
		if got := q.Pop(); got != w {
			t.Fatalf("第 %d 个出队的不对：期望优先级 %s，实际 %s", i+1, w.prio, got.prio)
		}
	}
	if q.Pop() != nil {
		t.Fatal("队列应已空")
	}
	if q.Len() != 0 {
		t.Fatalf("Len 应为 0，实际 %d", q.Len())
	}
}

func TestQueueRemoveOnlyTouchesTarget(t *testing.T) {
	q := &priorityQueue{}
	a := newWaiter(PriorityNormal)
	b := newWaiter(PriorityHigh)

	q.Push(a)
	q.Push(b)

	if !q.Remove(a) {
		t.Fatal("Remove 应成功")
	}
	if q.Remove(a) {
		t.Fatal("重复 Remove 应返回 false")
	}
	if q.Len() != 1 {
		t.Fatalf("Len 应为 1，实际 %d", q.Len())
	}
	if got := q.Pop(); got != b {
		t.Fatal("被摘除的不该是 b")
	}
}

func TestQueueDrainReturnsEverything(t *testing.T) {
	q := &priorityQueue{}
	for i := 0; i < 5; i++ {
		q.Push(newWaiter(Priority(i % priorityLevels)))
	}
	if got := len(q.drain()); got != 5 {
		t.Fatalf("drain 应返回 5 个，实际 %d", got)
	}
	if q.Len() != 0 {
		t.Fatal("drain 后队列应为空")
	}
}

// ==================== 并发池 ====================

func newTestPool(maxConcurrent, maxQueueDepth int, queueTimeout time.Duration) *Pool {
	return newPool("test-model",
		config.InferencePoolConfig{MaxConcurrent: maxConcurrent, Weight: 1},
		queueTimeout, maxQueueDepth)
}

func waitFor(t *testing.T, d time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return cond()
}

// TestPoolCapsConcurrencyAndQueuesExcess 验收"排队生效"：
// 并发上限不被突破，超出的请求进入队列，处理完后队列回落。
func TestPoolCapsConcurrencyAndQueuesExcess(t *testing.T) {
	const (
		maxConcurrent = 10
		total         = 100
	)
	p := newTestPool(maxConcurrent, 500, 5*time.Second)

	var (
		inflight    int32
		maxObserved int32
		maxQueued   int32
		wg          sync.WaitGroup
	)
	wg.Add(total)
	start := make(chan struct{})

	for i := 0; i < total; i++ {
		go func() {
			defer wg.Done()
			<-start
			if err := p.Acquire(context.Background()); err != nil {
				t.Errorf("Acquire 失败: %v", err)
				return
			}
			cur := atomic.AddInt32(&inflight, 1)
			for {
				old := atomic.LoadInt32(&maxObserved)
				if cur <= old || atomic.CompareAndSwapInt32(&maxObserved, old, cur) {
					break
				}
			}
			time.Sleep(5 * time.Millisecond)
			atomic.AddInt32(&inflight, -1)
			p.Release()
		}()
	}

	// 采集队列深度峰值
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			_, queued := p.Stats()
			for {
				old := atomic.LoadInt32(&maxQueued)
				if int32(queued) <= old || atomic.CompareAndSwapInt32(&maxQueued, old, int32(queued)) {
					break
				}
			}
			select {
			case <-time.After(time.Millisecond):
			default:
			}
			if atomic.LoadInt32(&inflight) == 0 && maxQueued > 0 && queued == 0 {
				return
			}
		}
	}()

	close(start)
	wg.Wait()
	<-done

	if maxObserved > maxConcurrent {
		t.Fatalf("并发放开上限：maxConcurrent=%d，实际峰值 %d", maxConcurrent, maxObserved)
	}
	if maxQueued == 0 {
		t.Fatal("100 个请求打 10 个槽位，应观察到队列深度上升")
	}

	// 全部处理完后必须回落
	if ok := waitFor(t, 2*time.Second, func() bool {
		inflight, queued := p.Stats()
		return inflight == 0 && queued == 0
	}); !ok {
		inflight, queued := p.Stats()
		t.Fatalf("队列应回落：inflight=%d queued=%d", inflight, queued)
	}
}

// TestPoolServesHighPriorityFirst 验收"优先级生效"：
// 只有 1 个槽位时，先排 Low 再排 High，释放后必须先服务 High。
func TestPoolServesHighPriorityFirst(t *testing.T) {
	p := newTestPool(1, 10, 5*time.Second)

	// 占住唯一的槽位
	if err := p.Acquire(context.Background()); err != nil {
		t.Fatalf("占位失败: %v", err)
	}

	var order []string
	var mu sync.Mutex
	record := func(s string) {
		mu.Lock()
		order = append(order, s)
		mu.Unlock()
	}

	lowDone := make(chan struct{})
	highDone := make(chan struct{})

	go func() {
		defer close(lowDone)
		if err := p.Acquire(WithPriority(context.Background(), PriorityLow)); err != nil {
			t.Errorf("Low 排队失败: %v", err)
			return
		}
		record("low")
		p.Release()
	}()

	// 等 Low 真的排进队列，再让 High 入队，保证顺序确定
	if !waitFor(t, time.Second, func() bool { _, q := p.Stats(); return q == 1 }) {
		t.Fatal("Low 未进入队列")
	}

	go func() {
		defer close(highDone)
		if err := p.Acquire(WithPriority(context.Background(), PriorityHigh)); err != nil {
			t.Errorf("High 排队失败: %v", err)
			return
		}
		record("high")
		p.Release()
	}()
	if !waitFor(t, time.Second, func() bool { _, q := p.Stats(); return q == 2 }) {
		t.Fatal("High 未进入队列")
	}

	// 释放槽位：应先给 High
	p.Release()

	select {
	case <-highDone:
	case <-time.After(2 * time.Second):
		t.Fatal("High 请求未被优先服务")
	}
	select {
	case <-lowDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Low 请求最终也应被服务")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(order) < 2 || order[0] != "high" {
		t.Fatalf("先被服务的应是 High，实际顺序 %v", order)
	}
}

// TestPoolRejectsWhenQueueFull 验收"排队上限"：队列打满后立刻拒绝。
func TestPoolRejectsWhenQueueFull(t *testing.T) {
	p := newTestPool(1, 2, 5*time.Second)

	if err := p.Acquire(context.Background()); err != nil {
		t.Fatalf("占位失败: %v", err)
	}

	// 占满两个排队位
	for i := 0; i < 2; i++ {
		go func() {
			_ = p.Acquire(context.Background())
		}()
	}
	if !waitFor(t, time.Second, func() bool { _, q := p.Stats(); return q == 2 }) {
		t.Fatal("队列未打满")
	}

	err := p.Acquire(context.Background())
	if !errorsIs(err, ErrQueueFull) {
		t.Fatalf("队列满时应返回 ErrQueueFull，实际: %v", err)
	}
}

// TestPoolRejectsOnQueueTimeout 验收"排队超时拒绝"。
func TestPoolRejectsOnQueueTimeout(t *testing.T) {
	p := newTestPool(1, 10, 60*time.Millisecond)

	if err := p.Acquire(context.Background()); err != nil {
		t.Fatalf("占位失败: %v", err)
	}

	start := time.Now()
	err := p.Acquire(context.Background())
	elapsed := time.Since(start)

	if !errorsIs(err, ErrQueueTimeout) {
		t.Fatalf("应返回 ErrQueueTimeout，实际: %v", err)
	}
	if elapsed > time.Second {
		t.Fatalf("应在超时后立即返回，实际耗时 %s", elapsed)
	}
	// 超时的请求必须把自己摘干净，否则会占着后续请求的名额
	if _, queued := p.Stats(); queued != 0 {
		t.Fatalf("超时请求应已出队，队列实际 %d", queued)
	}
}

// TestPoolReleaseAfterTimeoutDoesNotLeak 超时请求被 Release 选中时，
// 槽位必须被正确回收，不能出现"少还一次"。
func TestPoolReleaseAfterTimeoutDoesNotLeak(t *testing.T) {
	p := newTestPool(1, 10, 50*time.Millisecond)
	if err := p.Acquire(context.Background()); err != nil {
		t.Fatalf("占位失败: %v", err)
	}

	// 这个请求会超时，但可能刚好被 Release 选中
	_ = p.Acquire(context.Background())

	// 归还槽位；此时池子必须回到"空闲且可再次获取"
	p.Release()
	if ok := waitFor(t, time.Second, func() bool {
		inflight, _ := p.Stats()
		return inflight == 0
	}); !ok {
		inflight, queued := p.Stats()
		t.Fatalf("槽位未回收：inflight=%d queued=%d", inflight, queued)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := p.Acquire(ctx); err != nil {
		t.Fatalf("槽位应可再次获取: %v", err)
	}
	p.Release()
}

// TestPoolCloseRejectsNewAndDrainsInflight 验收"优雅关闭"：
// 关闭后新请求被拒（不是发放槽位），在途请求仍能正常结束。
func TestPoolCloseRejectsNewAndDrainsInflight(t *testing.T) {
	// maxConcurrent=1：第二个请求才会真的排队，而不是走快路径
	p := newTestPool(1, 10, time.Second)

	// 一个在途请求
	if err := p.Acquire(context.Background()); err != nil {
		t.Fatalf("占位失败: %v", err)
	}
	// 一个排队请求
	queued := make(chan error, 1)
	go func() { queued <- p.Acquire(context.Background()) }()
	if !waitFor(t, time.Second, func() bool { _, q := p.Stats(); return q == 1 }) {
		t.Fatal("排队请求未入队")
	}

	p.Close()

	select {
	case err := <-queued:
		if !errorsIs(err, ErrShuttingDown) {
			t.Fatalf("排队请求应收到 ErrShuttingDown，实际: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("关闭后排队请求未被唤醒")
	}

	if err := p.Acquire(context.Background()); !errorsIs(err, ErrShuttingDown) {
		t.Fatalf("关闭后新请求应被拒绝，实际: %v", err)
	}

	// 在途请求结束后 Drain 应返回 true
	p.Release()
	if !p.Drain(time.Second) {
		t.Fatal("在途请求结束后 Drain 应返回 true")
	}
}

// errorsIs 是 errors.Is 的短别名，省得每个断言都写完整包名。
func errorsIs(err, target error) bool { return errors.Is(err, target) }
