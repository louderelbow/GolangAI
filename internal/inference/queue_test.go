package inference

import (
	"context"
	"testing"
	"time"

	"deeptalk/internal/infra/config"
)

// TestPriorityFromContext 未标记时默认 Normal。
//
// 这一条保护的是"忘记标记"的后果可接受：新加的调用点就算忘了设优先级，
// 也只是走普通队列，不会被误判成 Low 而被饿死。
func TestPriorityFromContext(t *testing.T) {
	if got := PriorityFrom(context.Background()); got != PriorityNormal {
		t.Errorf("未标记时应当默认 Normal，实际 %s", got)
	}

	ctx := WithPriority(context.Background(), PriorityHigh)
	if got := PriorityFrom(ctx); got != PriorityHigh {
		t.Errorf("标记应当生效，实际 %s", got)
	}
}

// TestPriorityQueuePopsHighFirst 级间抢占：高优先级后进也能先出。
//
// 这是整个优先级机制的核心断言。没有它，"优先级队列"就只是三个切片。
func TestPriorityQueuePopsHighFirst(t *testing.T) {
	q := &priorityQueue{}

	low := newWaiter(PriorityLow)
	normal := newWaiter(PriorityNormal)
	high := newWaiter(PriorityHigh)

	// 刻意按 low → normal → high 的顺序入队，验证高优先级能"插队"
	q.Push(low)
	q.Push(normal)
	q.Push(high)

	if got := q.Pop(); got != high {
		t.Errorf("第一个出队的应当是 high，实际 %s", got.prio)
	}
	if got := q.Pop(); got != normal {
		t.Errorf("第二个出队的应当是 normal，实际 %s", got.prio)
	}
	if got := q.Pop(); got != low {
		t.Errorf("第三个出队的应当是 low，实际 %s", got.prio)
	}
	if got := q.Pop(); got != nil {
		t.Errorf("队列空了应当返回 nil，实际 %v", got)
	}
}

// TestPriorityQueueFIFOWithinLevel 级内必须是严格先进先出。
//
// 同优先级的请求之间不该有"后来居上" —— 那会让先到的请求被无限饿死。
func TestPriorityQueueFIFOWithinLevel(t *testing.T) {
	q := &priorityQueue{}

	first := newWaiter(PriorityNormal)
	second := newWaiter(PriorityNormal)
	third := newWaiter(PriorityNormal)

	q.Push(first)
	q.Push(second)
	q.Push(third)

	for i, want := range []*waiter{first, second, third} {
		if got := q.Pop(); got != want {
			t.Fatalf("第 %d 个出队的应当是第 %d 个入队的（级内 FIFO）", i+1, i+1)
		}
	}
}

// TestPriorityQueueRemoveKeepsOrder 摘掉一个之后，其余的顺序不能乱。
func TestPriorityQueueRemoveKeepsOrder(t *testing.T) {
	q := &priorityQueue{}

	a := newWaiter(PriorityNormal)
	b := newWaiter(PriorityNormal)
	c := newWaiter(PriorityNormal)
	q.Push(a)
	q.Push(b)
	q.Push(c)

	if !q.Remove(b) {
		t.Fatal("b 在队列里，应当能摘掉")
	}
	if q.Len() != 2 {
		t.Fatalf("摘掉一个后长度应当为 2，实际 %d", q.Len())
	}
	if got := q.Pop(); got != a {
		t.Error("摘掉中间一个之后，队首仍应是 a")
	}
	if got := q.Pop(); got != c {
		t.Error("接着应当出 c")
	}
	// 已经摘掉的不能再摘一次
	if q.Remove(b) {
		t.Error("b 已经不在队列里，Remove 应当返回 false")
	}
}

// TestPoolRecordsPriority 入队时优先级被正确记录到对应的那一级。
//
// 用 snapshot 而不是靠 goroutine 竞速，是为了让测试**确定性**通过 ——
// 靠 sleep 排时序的并发测试迟早会在 CI 上随机红。
func TestPoolRecordsPriority(t *testing.T) {
	// queueTimeout 必须给一个**真实**的值：传 0 的话定时器立刻触发，
	// 排队者一进队列就超时退出，队列永远是空的（这个坑我自己踩过一次）。
	p := newPool("test", config.InferencePoolConfig{MaxConcurrent: 1, Weight: 1}, 5*time.Second, 10)
	defer p.Close()

	// 占满唯一的槽位，逼后续请求排队
	if err := p.Acquire(context.Background()); err != nil {
		t.Fatalf("占位失败: %v", err)
	}

	// 三个不同优先级的请求依次排队（Acquire 会阻塞，所以放 goroutine）
	for _, prio := range []Priority{PriorityLow, PriorityHigh, PriorityNormal} {
		go func(pr Priority) {
			ctx := WithPriority(context.Background(), pr)
			_ = p.Acquire(ctx)
		}(prio)
	}

	// 等三个都排进队列。
	//
	// 用"轮询到满足条件"而不是 sleep 一个固定值：固定 sleep 在慢机器上会偶发失败，
	// 而这类并发测试一旦开始随机红，最后一定会被人加个 skip 然后忘掉。
	deadline := time.Now().Add(2 * time.Second)
	var snap [priorityLevels]int
	for time.Now().Before(deadline) {
		p.mu.Lock()
		snap = p.waiters.snapshot()
		p.mu.Unlock()
		if snap[0]+snap[1]+snap[2] == 3 {
			break
		}
		time.Sleep(time.Millisecond)
	}

	// snapshot[0]=high, [1]=normal, [2]=low
	if snap[PriorityHigh] != 1 || snap[PriorityNormal] != 1 || snap[PriorityLow] != 1 {
		t.Fatalf("三个优先级应当各有一个排队者，实际 high=%d normal=%d low=%d",
			snap[PriorityHigh], snap[PriorityNormal], snap[PriorityLow])
	}
}
