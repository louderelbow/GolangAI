package inference

import (
	"sort"
	"sync"
	"sync/atomic"
)

// Instance 同一个模型的一个上游部署点。
//
// 当前部署每个模型只有一个上游（ragModelConfig 里的 baseURL），
// 因此 instances 通常长度为 1，Balancer 退化为"永远选它"。
// 这个抽象是为"同一模型挂多个上游/多副本"预留的扩展点，
// 也是 weight 配置真正生效的地方——单实例时 weight 无意义。
type Instance struct {
	ID     string
	Weight int

	inflight int64
}

func (i *Instance) acquire() { atomic.AddInt64(&i.inflight, 1) }
func (i *Instance) release() { atomic.AddInt64(&i.inflight, -1) }

// Inflight 返回当前在途请求数。
func (i *Instance) Inflight() int64 { return atomic.LoadInt64(&i.inflight) }

// Balancer 在多个实例之间分发请求。
type Balancer interface {
	// Pick 选一个实例并占位；无实例时返回 nil
	Pick() *Instance
	// Release 归还占位
	Release(inst *Instance)
}

// NewBalancer 按策略名构造均衡器。
//
// 支持 "least_conn"（默认）与 "weighted"；
// 单实例时两者行为完全一致（都只有唯一选择）。
func NewBalancer(strategy string, instances []*Instance) Balancer {
	valid := make([]*Instance, 0, len(instances))
	for _, in := range instances {
		if in != nil {
			if in.Weight <= 0 {
				in.Weight = 1
			}
			valid = append(valid, in)
		}
	}
	if strategy == "weighted" {
		return &smoothWRR{instances: valid, current: make([]int, len(valid))}
	}
	return &leastConn{instances: valid}
}

// ==================== 最少连接 ====================

type leastConn struct {
	mu        sync.Mutex
	instances []*Instance
}

// Pick 选在途最少的实例；并列时按 ID 稳定排序，避免每次都挑到同一个。
func (b *leastConn) Pick() *Instance {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.instances) == 0 {
		return nil
	}

	best := b.instances[0]
	for _, in := range b.instances[1:] {
		if in.Inflight() < best.Inflight() ||
			(in.Inflight() == best.Inflight() && in.ID < best.ID) {
			best = in
		}
	}
	best.acquire()
	return best
}

func (b *leastConn) Release(inst *Instance) {
	if inst != nil {
		inst.release()
	}
}

// ==================== 平滑加权轮询 ====================

// smoothWRR 是 Nginx 同款的平滑加权轮询：
// 每轮给每个实例的当前权重加上自身权重，选最大者，再减去总权重。
// 相比"按权重连续发 N 个"的朴素做法，它不会把同一实例的请求挤在一起。
type smoothWRR struct {
	mu        sync.Mutex
	instances []*Instance
	current   []int
}

func (b *smoothWRR) Pick() *Instance {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.instances) == 0 {
		return nil
	}

	total := 0
	for i, in := range b.instances {
		b.current[i] += in.Weight
		total += in.Weight
	}

	bestIdx := 0
	for i := 1; i < len(b.instances); i++ {
		if b.current[i] > b.current[bestIdx] {
			bestIdx = i
		}
	}
	b.current[bestIdx] -= total

	b.instances[bestIdx].acquire()
	return b.instances[bestIdx]
}

func (b *smoothWRR) Release(inst *Instance) {
	if inst != nil {
		inst.release()
	}
}

// SortInstancesByID 让实例顺序稳定，便于测试与日志对比。
func SortInstancesByID(instances []*Instance) {
	sort.Slice(instances, func(i, j int) bool { return instances[i].ID < instances[j].ID })
}
