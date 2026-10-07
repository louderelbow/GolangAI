package cache

import (
	"container/list"
	"hash/fnv"
	"math/rand"
	"strconv"
	"sync"
	"time"
)

// ======================== L1 精确缓存 ========================
//
// 命中条件：prompt 完全一致（模型 + 问题 的 hash）。
//
// 为什么它不是"语义缓存的重复"——语义缓存每次查询都要算一次 embedding：
//
//	L1 命中 = 0 次模型调用 + 0 次 embedding 调用
//	L2 命中 = 0 次模型调用 + 1 次 embedding 调用
//
// 也就是说"一模一样的问题"走 L1 能省掉一次外部调用。而真实流量里，
// 重复提问（用户连点、前端重试、同一批人问同一件事）恰恰最常见。
//
// 存储用进程内 LRU：这一层要的就是"零成本"，走 Redis 反而多一次网络往返。

type exactEntry struct {
	key     string
	answer  string
	expires time.Time

	// 命中时"省下来"的量：回源那次实际消耗的 token 与费用。
	// 记在条目上而不是用固定估值，这样 saved_tokens 指标才是有依据的。
	tokens     int
	costMicros int64
}

type exactCache struct {
	mu    sync.Mutex
	ll    *list.List               // 队首 = 最近使用
	index map[string]*list.Element // key -> element（配合 list 做 O(1) 淘汰）
	max   int
}

func newExactCache(max int) *exactCache {
	if max <= 0 {
		max = 10000
	}
	return &exactCache{ll: list.New(), index: make(map[string]*list.Element, max), max: max}
}

// exactKey 把"模型 + 问题"折叠成一个稳定 key。
//
// 带上模型名是必须的：同一个问题在 RAG 和 Agent 下的答案完全不同，
// 只按问题做 key 会把两种模型的答案串起来。
func exactKey(modelKey, question string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(modelKey))
	_, _ = h.Write([]byte{0}) // 分隔符，避免 "a"+"bc" 与 "ab"+"c" 撞成同一个 key
	_, _ = h.Write([]byte(question))
	return strconv.FormatUint(h.Sum64(), 16)
}

// Get 命中则返回值并刷新 LRU 位置。
func (c *exactCache) Get(modelKey, question string) (string, int, int64, bool) {
	key := exactKey(modelKey, question)

	c.mu.Lock()
	defer c.mu.Unlock()

	el, ok := c.index[key]
	if !ok {
		return "", 0, 0, false
	}
	e := el.Value.(*exactEntry)
	if time.Now().After(e.expires) {
		// 过期即删，避免"读到的都是过期数据"
		c.removeElement(el)
		return "", 0, 0, false
	}
	c.ll.MoveToFront(el)
	return e.answer, e.tokens, e.costMicros, true
}

// Set 写入并带上 TTL 抖动。
//
// 抖动是防雪崩：如果一批 key 同时写入、TTL 又完全一样，
// 它们会在同一秒集体失效，流量瞬间全打到模型上。
func (c *exactCache) Set(modelKey, question, answer string, tokens int, costMicros int64, ttl time.Duration, jitter float64) {
	if answer == "" {
		return
	}
	key := exactKey(modelKey, question)
	entry := &exactEntry{
		key:        key,
		answer:     answer,
		expires:    time.Now().Add(ttlWithJitter(ttl, jitter)),
		tokens:     tokens,
		costMicros: costMicros,
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if el, ok := c.index[key]; ok {
		el.Value = entry
		c.ll.MoveToFront(el)
		return
	}
	c.index[key] = c.ll.PushFront(entry)
	for c.ll.Len() > c.max {
		if oldest := c.ll.Back(); oldest != nil {
			c.removeElement(oldest)
		}
	}
}

// Delete 删除单条（缓存一致性：写操作后要让旧答案失效）。
func (c *exactCache) Delete(modelKey, question string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.index[exactKey(modelKey, question)]; ok {
		c.removeElement(el)
	}
}

// Purge 清空。
func (c *exactCache) Purge() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ll.Init()
	c.index = make(map[string]*list.Element, c.max)
}

// Len 当前条数（可观测性）。
func (c *exactCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ll.Len()
}

// removeElement 调用方必须已持有锁。
func (c *exactCache) removeElement(el *list.Element) {
	c.ll.Remove(el)
	delete(c.index, el.Value.(*exactEntry).key)
}

// ttlWithJitter 给 TTL 加 ±jitter 的随机抖动（jitter 为比例，如 0.2 = ±20%）。
//
// 这是防雪崩的核心手段：让同一批写入的 key 不在同一时刻集体过期。
func ttlWithJitter(ttl time.Duration, jitter float64) time.Duration {
	if ttl <= 0 {
		return ttl
	}
	if jitter <= 0 {
		return ttl
	}
	if jitter > 1 {
		jitter = 1
	}
	// [-1, 1] * jitter
	factor := 1 + (rand.Float64()*2-1)*jitter
	return time.Duration(float64(ttl) * factor)
}
