package aihelper

import (
	"context"
	"deeptalk/common/metrics"
	"deeptalk/common/resilience"
	"deeptalk/config"
	"fmt"
	"log"
	"math"
	"os"
	"sync"
	"time"

	embeddingArk "github.com/cloudwego/eino-ext/components/embedding/ark"
	"github.com/cloudwego/eino/components/embedding"
)

// ======================== 语义缓存 ========================

// 存储：进程内、有上限、带 TTL。多实例部署时各处一份（可换成 Redis 向量库）。
// 降级：embedding 调用失败时直接放行（不缓存、不命中），不影响主流程。

type semEntry struct {
	question string
	answer   string
	vector   []float64
	model    string
	created  time.Time
}

type semanticCache struct {
	mu      sync.Mutex
	entries []*semEntry

	embedOnce sync.Once
	embedder  embedding.Embedder
	embedErr  error
}

var globalSemanticCache = &semanticCache{}

// semanticEmbedTimeout 语义缓存 embedding 的超时：超了就放弃缓存，不能拖慢主流程
const semanticEmbedTimeout = 3 * time.Second

// GetSemanticCache 全局语义缓存
func GetSemanticCache() *semanticCache { return globalSemanticCache }

func (c *semanticCache) cfg() config.SemanticCacheConfig {
	return config.GetConfig().SemanticCache
}

func (c *semanticCache) Enabled() bool {
	cfg := c.cfg()
	return cfg.Enabled
}

func (c *semanticCache) threshold() float64 {
	t := c.cfg().Threshold
	if t <= 0 || t > 1 {
		t = 0.92
	}
	return t
}

func (c *semanticCache) ttl() time.Duration {
	sec := c.cfg().TTLSeconds
	if sec <= 0 {
		sec = 3600
	}
	return time.Duration(sec) * time.Second
}

func (c *semanticCache) maxEntries() int {
	n := c.cfg().MaxEntries
	if n <= 0 {
		n = 500
	}
	return n
}

// embedOne 计算文本向量（embedder 只建一次，避免每次请求都新建客户端）
func (c *semanticCache) embedOne(ctx context.Context, text string) ([]float64, error) {
	c.embedOnce.Do(func() {
		cfg := config.GetConfig()
		key := cfg.RagModelConfig.RagApiKey
		if key == "" {
			key = os.Getenv("ALIYUN_API_KEY")
		}
		if key == "" {
			key = os.Getenv("DEEPSEEK_API_KEY")
		}
		c.embedder, c.embedErr = embeddingArk.NewEmbedder(ctx, &embeddingArk.EmbeddingConfig{
			BaseURL: cfg.RagModelConfig.RagBaseUrl,
			APIKey:  key,
			Model:   cfg.RagModelConfig.RagEmbeddingModel,
		})
	})
	if c.embedErr != nil {
		return nil, c.embedErr
	}

	embedCtx, cancel := context.WithTimeout(ctx, semanticEmbedTimeout)
	defer cancel()

	vecs, err := resilience.Do(resilience.HTTPKey("embedding"), func() ([][]float64, error) {
		return c.embedder.EmbedStrings(embedCtx, []string{text})
	})
	if err != nil {
		return nil, err
	}
	if len(vecs) == 0 || len(vecs[0]) == 0 {
		return nil, fmt.Errorf("empty embedding result")
	}
	return vecs[0], nil
}

func cosine(a, b []float64) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return -1
	}
	var dot, na, nb float64
	for i := range a {
		dot += a[i] * b[i]
		na += a[i] * a[i]
		nb += b[i] * b[i]
	}
	if na == 0 || nb == 0 {
		return -1
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// Lookup 查询语义缓存；命中返回答案
func (c *semanticCache) Lookup(ctx context.Context, modelKey, question string) (string, bool) {
	if !c.Enabled() {
		return "", false
	}

	vec, err := c.embedOne(ctx, question)
	if err != nil {
		// embedding 不可用时静默跳过缓存（不影响正常问答）
		log.Printf("[SemanticCache] embed failed, skip cache: %v", err)
		return "", false
	}

	threshold := c.threshold()
	ttl := c.ttl()
	now := time.Now()

	c.mu.Lock()
	defer c.mu.Unlock()

	best := -1.0
	var hit *semEntry
	for _, e := range c.entries {
		if e.model != modelKey || now.Sub(e.created) > ttl {
			continue
		}
		if sim := cosine(vec, e.vector); sim > best {
			best = sim
			hit = e
		}
	}

	if hit != nil && best >= threshold {
		metrics.RecordCacheLookup(true)
		log.Printf("[SemanticCache] HIT sim=%.4f question=%.30s", best, question)
		return hit.answer, true
	}

	metrics.RecordCacheLookup(false)
	return "", false
}

// Store 写入语义缓存
func (c *semanticCache) Store(ctx context.Context, modelKey, question, answer string) {
	if !c.Enabled() || answer == "" {
		return
	}

	vec, err := c.embedOne(ctx, question)
	if err != nil {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// 先清理过期项
	ttl := c.ttl()
	now := time.Now()
	alive := c.entries[:0]
	for _, e := range c.entries {
		if now.Sub(e.created) <= ttl {
			alive = append(alive, e)
		}
	}
	c.entries = alive

	// 超上限时淘汰最旧的一条
	if len(c.entries) >= c.maxEntries() {
		c.entries = c.entries[1:]
	}

	c.entries = append(c.entries, &semEntry{
		question: question,
		answer:   answer,
		vector:   vec,
		model:    modelKey,
		created:  now,
	})
}

// Stats 缓存规模（可观测性）
func (c *semanticCache) Stats() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}
