package inference

import (
	"context"
	"time"
)

// Priority 请求优先级：数值越小越优先。
type Priority int

const (
	PriorityHigh Priority = iota
	PriorityNormal
	PriorityLow

	priorityLevels = 3
)

func (p Priority) String() string {
	switch p {
	case PriorityHigh:
		return "high"
	case PriorityLow:
		return "low"
	default:
		return "normal"
	}
}

type priorityKey struct{}

// WithPriority 在请求上下文里标记优先级。
func WithPriority(ctx context.Context, p Priority) context.Context {
	return context.WithValue(ctx, priorityKey{}, p)
}

// PriorityFrom 读取上下文里的优先级，未标记时按 Normal 处理。
func PriorityFrom(ctx context.Context) Priority {
	if p, ok := ctx.Value(priorityKey{}).(Priority); ok {
		return p
	}
	return PriorityNormal
}

// waiter 一个正在排队的请求。
//
// ready 是容量 1 的缓冲通道：槽位从 Release 侧"直接转交"过来，
// 发送方永远不会阻塞。err 在发送前写入，接收方通过通道获得 happens-before，
// 因此不需要额外加锁。
type waiter struct {
	prio     Priority
	ready    chan struct{}
	enqueued time.Time
	err      error // nil = 拿到槽位；非 nil = 未拿到（如关闭中）
}

func newWaiter(p Priority) *waiter {
	return &waiter{prio: p, ready: make(chan struct{}, 1), enqueued: time.Now()}
}

// priorityQueue 三级优先级队列：级间抢占，级内 FIFO。
//
// 用三个切片而不是堆：只有固定三级，且每级内部就是先进先出，
// 三切片的出队是 O(1)，读起来也比堆直观。
type priorityQueue struct {
	levels [priorityLevels][]*waiter
	n      int
}

func (q *priorityQueue) Push(w *waiter) {
	q.levels[w.prio] = append(q.levels[w.prio], w)
	q.n++
}

// Pop 取出当前最高优先级的队首；队列为空返回 nil。
func (q *priorityQueue) Pop() *waiter {
	for p := 0; p < priorityLevels; p++ {
		if len(q.levels[p]) > 0 {
			w := q.levels[p][0]
			q.levels[p] = q.levels[p][1:]
			q.n--
			return w
		}
	}
	return nil
}

// Remove 从队列里精确摘掉某个 waiter。
//
// 排队超时的请求必须能摘掉自己，否则"已经放弃的请求"还会占着队列名额，
// 让后来者被误判为 queue_full。返回 false 表示它已经被 Release 选中。
func (q *priorityQueue) Remove(target *waiter) bool {
	for p := 0; p < priorityLevels; p++ {
		for i, w := range q.levels[p] {
			if w == target {
				q.levels[p] = append(q.levels[p][:i], q.levels[p][i+1:]...)
				q.n--
				return true
			}
		}
	}
	return false
}

func (q *priorityQueue) Len() int { return q.n }

// drain 清空队列并返回全部等待者（关闭时用来一次性唤醒）。
func (q *priorityQueue) drain() []*waiter {
	out := make([]*waiter, 0, q.n)
	for p := 0; p < priorityLevels; p++ {
		out = append(out, q.levels[p]...)
		q.levels[p] = nil
	}
	q.n = 0
	return out
}

// snapshot 按优先级返回各级排队长度，用于断言与日志。
func (q *priorityQueue) snapshot() [priorityLevels]int {
	var out [priorityLevels]int
	for p := 0; p < priorityLevels; p++ {
		out[p] = len(q.levels[p])
	}
	return out
}
