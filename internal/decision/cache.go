package decision

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"deeptalk/internal/infra/config"
	"deeptalk/internal/infra/metrics"

	"github.com/cloudwego/eino-ext/components/model/openai"
	einomodel "github.com/cloudwego/eino/components/model"
)

// ---------------- 缓存 ----------------

type intentCacheEntry struct {
	result  IntentResult
	expires time.Time
}

var (
	intentCacheMu sync.Mutex
	intentCache   = map[string]intentCacheEntry{}
)

func intentCacheGet(key string) (IntentResult, bool) {
	intentCacheMu.Lock()
	defer intentCacheMu.Unlock()
	e, ok := intentCache[key]
	if !ok || time.Now().After(e.expires) {
		if ok {
			delete(intentCache, key)
		}
		return IntentResult{}, false
	}
	return e.result, true
}

// resetIntentCache 清空意图缓存（测试用：缓存是全局状态，测试之间需要隔离）
func resetIntentCache() {
	intentCacheMu.Lock()
	defer intentCacheMu.Unlock()
	intentCache = map[string]intentCacheEntry{}
}

func intentCacheSet(key string, r IntentResult, ttl time.Duration) {
	cfg := config.GetConfig().IntentConfig
	max := cfg.MaxCacheEntries
	if max <= 0 {
		max = 1000
	}

	intentCacheMu.Lock()
	defer intentCacheMu.Unlock()
	if len(intentCache) >= max {
		// 简单的清空策略：容量满就重置（意图缓存本身是加速项，丢了不影响正确性）
		intentCache = map[string]intentCacheEntry{}
	}
	intentCache[key] = intentCacheEntry{result: r, expires: time.Now().Add(ttl)}
}

// ---------------- 对外入口 ----------------

func ClassifyIntent(ctx context.Context, question string, llm intentLLM) IntentResult {
	cfg := config.GetConfig().IntentConfig

	res, confident := ruleIntent(question)
	if confident || !cfg.LLMFallback {
		recordIntent(res)
		return res
	}
	// 缓存：同一问题短时间内只判一次
	cacheKey := strings.TrimSpace(strings.ToLower(question))
	ttl := time.Duration(cfg.CacheTTLSeconds) * time.Second
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	if cached, ok := intentCacheGet(cacheKey); ok {
		recordIntent(cached)
		return cached
	}

	final := llmIntent(ctx, question, llm, res.Intent)
	if final.Layer == LayerLLM {
		final.Score = res.Score
	}
	intentCacheSet(cacheKey, final, ttl)
	recordIntent(final)
	return final
}

func recordIntent(r IntentResult) {
	metrics.Count("deeptalk_intent_total", metrics.Labels{"intent": string(r.Intent), "layer": r.Layer}, 1)
	log.Printf("[Intent] intent=%s layer=%s score=%d reason=%s", r.Intent, r.Layer, r.Score, r.Reason)
}

// IntentMetricsName 供文档/看板引用的指标名
const IntentMetricsName = "deeptalk_intent_total"

func NewIntentLLM(ctx context.Context) (einomodel.ToolCallingChatModel, error) {
	cfg := config.GetConfig()
	key := cfg.RagApiKey()
	return openai.NewChatModel(ctx, &openai.ChatModelConfig{
		BaseURL: cfg.RagModelConfig.RagBaseUrl,
		Model:   cfg.RagModelConfig.RagChatModelName,
		APIKey:  key,
	})
}

// RuleIntentOnly 只跑规则层，返回判定结果
func RuleIntentOnly(question string) IntentResult {
	res, _ := ruleIntent(question)
	return res
}

// RuleIntentWithConfidence 返回规则层结果与"是否高置信度"
func RuleIntentWithConfidence(question string) (IntentResult, bool) {
	return ruleIntent(question)
}

// AllIntents 全部候选意图（评测用）
func AllIntents() []Intent { return []Intent{IntentSummary, IntentQuestion, IntentChat} }

var _ = fmt.Sprintf // 保留 fmt 依赖（错误信息拼接用）
