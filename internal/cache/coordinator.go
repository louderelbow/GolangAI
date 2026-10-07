package cache

import (
	"context"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"deeptalk/internal/infra/config"
	"deeptalk/internal/infra/metrics"

	"golang.org/x/sync/singleflight"
)

// 命中层级，用于指标与日志。
const (
	LevelExact    = "exact"    // L1 精确
	LevelSemantic = "semantic" // L2 语义
	LevelNegative = "negative" // 空结果冷却期（穿透防护）
	LevelMiss     = "miss"     // 未命中
)

// Hit 一次缓存查询的结果。
type Hit struct {
	Answer string
	Level  string
	OK     bool

	// 回源那次实际消耗的 token 与费用——命中时这就是"省下来的量"
	Tokens     int
	CostMicros int64
}

// Source 回源产物：答案 + 它的代价。
type Source struct {
	Answer     string
	Tokens     int
	CostMicros int64
}

// Coordinator 多级答案缓存的统一入口。
//
//	查询：L1 精确 →（未命中）L2 语义 →（未命中）回源
//	写入：L1 + L2 同时写；拒答只写"空结果冷却期"，且 TTL 短得多
//
// 分层的意义在于成本，而不是命中率：
//
//	L1 命中 = 0 次模型调用 + 0 次 embedding 调用
//	L2 命中 = 0 次模型调用 + 1 次 embedding 调用
//
// 语义缓存必须先把问题向量化才能比相似度，这一步本身就是外部调用；
// L1 用完全一致的 hash 绕过了它。
type Coordinator struct {
	exact *exactCache
	neg   *negativeCache

	// group 做请求合并（击穿防护）：同一个 key 的并发回源只真正执行一次
	group singleflight.Group
}

var (
	globalCoordinator *Coordinator
	coordinatorOnce   sync.Once
)

// GetCoordinator 全局协调器（首次调用时按配置初始化）。
func GetCoordinator() *Coordinator {
	coordinatorOnce.Do(func() {
		cfg := config.GetConfig().GetCache()
		globalCoordinator = &Coordinator{
			exact: newExactCache(cfg.ExactMaxEntries),
			neg:   newNegativeCache(time.Duration(cfg.Penetration.EmptyResultTTLSeconds) * time.Second),
		}
	})
	return globalCoordinator
}

// Key 是答案缓存的命名空间。各级缓存的内部实现都只认这一个串。
//
// 为什么 User 必须在里面：RAG 的答案取决于**这个用户自己的文档目录**，
// Agent 的工具与数据也按用户装配。缓存键里不带用户时，A 问出来的答案
// 会被 B 的同一句话命中——那是跨租户泄漏，不是"命中率高"。
// 所以这个字段不是可选优化，是隔离边界。
type Key struct {
	// User 必填。为空时整条缓存链路直接跳过（fail closed）：
	// 身份不明时宁可不缓存，也绝不能共用。
	User string
	// Model 形如 "6|deepseek-chat"：同一个问题在不同模型下的答案完全不同。
	Model string
}

// namespace 把 Key 折叠成内部各层使用的命名空间串；
// 返回空串表示"这次调用不可缓存"。
func (k Key) namespace() string {
	user := strings.TrimSpace(k.User)
	if user == "" {
		return ""
	}
	// 用户与模型之间用 '|' 分隔，而 exactKey 内部还会再插一个 \0——
	// 分隔符不同是为了避免 ("a","b|c") 与 ("a|b","c") 撞成同一个命名空间。
	return user + "|" + k.Model
}

// Lookup 依次查 L1 → L2。
func (c *Coordinator) Lookup(ctx context.Context, key Key, question string) Hit {
	ns := key.namespace()
	if ns == "" {
		// 没有用户身份就没有隔离边界，直接当作未命中
		return Hit{Level: LevelMiss}
	}

	cfg := config.GetConfig().GetCache()
	if !cfg.IsEnabled() {
		return Hit{Level: LevelMiss}
	}

	// L1：完全一致的 prompt，零外部调用
	if ans, tokens, cost, ok := c.exact.Get(ns, question); ok {
		metrics.CountCacheHit(LevelExact)
		return Hit{Answer: ans, Level: LevelExact, OK: true, Tokens: tokens, CostMicros: cost}
	}
	metrics.CountCacheMiss(LevelExact)

	// 穿透防护：刚问过且当时没有答案，短期内不必再跑一遍检索 + 模型
	if ans, ok := c.neg.Get(ns, question); ok {
		metrics.CountCachePenetrationBlocked()
		return Hit{Answer: ans, Level: LevelNegative, OK: true}
	}

	// L2：语义相近即可命中，但这一次查表要先算 embedding
	if ans, tokens, cost, ok := GetSemanticCache().Lookup(ctx, ns, question); ok {
		metrics.CountCacheHit(LevelSemantic)
		return Hit{Answer: ans, Level: LevelSemantic, OK: true, Tokens: tokens, CostMicros: cost}
	}
	metrics.CountCacheMiss(LevelSemantic)
	return Hit{Level: LevelMiss}
}

// Store 写入缓存。
//
// 拒答只进"空结果冷却期"：它不该占用正常答案的位置，
// 也不该活 30 分钟——用户补上文档后应当很快恢复。
func (c *Coordinator) Store(ctx context.Context, key Key, question string, src Source) {
	ns := key.namespace()
	if ns == "" {
		return
	}

	cfg := config.GetConfig().GetCache()
	if !cfg.IsEnabled() || src.Answer == "" {
		return
	}

	if IsRefusal(src.Answer) {
		c.neg.Store(ns, question, src.Answer)
		return
	}

	c.exact.Set(ns, question, src.Answer, src.Tokens, src.CostMicros,
		time.Duration(cfg.ExactTTLSeconds)*time.Second, cfg.JitterRatio)
	GetSemanticCache().Store(ctx, ns, question, src.Answer, src.Tokens, src.CostMicros)
	c.neg.Clear(ns, question)

	// 顺手刷新规模指标：缓存条数只在写入时变化，放在这里比开一个定时器便宜
	c.ReportStats()
}

// cachedAnswer singleflight 的执行结果：既可能是回源产物，也可能是等锁期间
// 别人已经写入的缓存。
type cachedAnswer struct {
	hit Hit
}

// Do 封装"查缓存 → 未命中则回源 → 写缓存"，并对同一个 key 做请求合并。
//
// 击穿防护：热点 key 刚失效的那一瞬间，并发请求会一起穿透到模型。
// singleflight 让同一个 key 的并发回源只真正执行一次，其余共享结果——
// 否则"缓存失效"会变成"缓存失效 + 模型被打 N 次"。
func (c *Coordinator) Do(
	ctx context.Context,
	key Key,
	question string,
	gen func() (Source, error),
) (Hit, error) {
	if hit := c.Lookup(ctx, key, question); hit.OK {
		c.recordSaving(hit)
		return hit, nil
	}

	ns := key.namespace()
	if ns == "" {
		// 不可缓存：直接回源，连 singleflight 都不用（合并的前提是有共同 key）
		src, err := gen()
		if err != nil {
			return Hit{Level: LevelMiss}, err
		}
		return Hit{Answer: src.Answer, Level: LevelMiss, OK: true}, nil
	}

	key2 := exactKey(ns, question)
	start := time.Now()

	// ran 标记"本次调用是否真的执行了回源"。用 atomic 是因为闭包可能
	// 跑在另一个 goroutine 上（singleflight 会把并发请求合并到先到的那个）。
	var ran atomic.Bool

	v, err, _ := c.group.Do(key2, func() (any, error) {
		ran.Store(true)
		// 双检：等锁期间可能已经有人写好了缓存
		if hit := c.Lookup(ctx, key, question); hit.OK {
			return cachedAnswer{hit: hit}, nil
		}
		src, genErr := gen()
		if genErr != nil {
			return nil, genErr
		}
		c.Store(ctx, key, question, src)
		return cachedAnswer{hit: Hit{Answer: src.Answer, Level: LevelMiss, OK: true}}, nil
	})

	if !ran.Load() {
		// 本次请求没有执行回源，纯粹在等别人——这才是真正的"锁等待"
		metrics.ObserveCacheLockWait(time.Since(start))
	}

	if err != nil {
		return Hit{Level: LevelMiss}, err
	}
	res, ok := v.(cachedAnswer)
	if !ok {
		return Hit{Level: LevelMiss}, nil
	}
	c.recordSaving(res.hit)
	return res.hit, nil
}

// recordSaving 命中时上报"省下的 token 与费用"。
func (c *Coordinator) recordSaving(hit Hit) {
	if hit.Level == LevelExact || hit.Level == LevelSemantic {
		metrics.AddCacheSaved(hit.Tokens, hit.CostMicros)
	}
}

// Invalidate 让某个问题的缓存失效（缓存一致性）。
//
// 触发点：用户重新上传/删除文档。此时旧答案可能已经不对，
// 而且此前"文档里没有答案"的判断也不再成立。
func Invalidate(key Key, question string) {
	ns := key.namespace()
	if ns == "" {
		return
	}
	c := GetCoordinator()
	c.exact.Delete(ns, question)
	c.neg.Clear(ns, question)
	log.Printf("[Cache] invalidated user=%s model=%s question=%.30s", key.User, key.Model, question)
}

// InvalidateAll 清空本地两级缓存（上传文档后调用）。
//
// 语义缓存是按向量近邻查的，无法精确知道"哪些答案受这篇文档影响"，
// 因此整体失效——文档更新是低频操作，整体失效换来的是"绝不会用到过期答案"。
func InvalidateAll() {
	c := GetCoordinator()
	c.exact.Purge()
	c.neg.Purge()
	GetSemanticCache().Purge()
	log.Printf("[Cache] all entries purged")
}

// Stats 各级缓存规模（可观测性）。
func (c *Coordinator) Stats() (exact, negative, semantic int) {
	return c.exact.Len(), c.neg.Len(), GetSemanticCache().Stats()
}

// ReportStats 把缓存规模写进指标，便于在 /metrics 上直接看。
func (c *Coordinator) ReportStats() {
	exact, negative, semantic := c.Stats()
	metrics.SetCacheSize(LevelExact, exact)
	metrics.SetCacheSize(LevelSemantic, semantic)
	metrics.SetCacheSize(LevelNegative, negative)
}
