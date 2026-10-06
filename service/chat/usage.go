package chat

import (
	"log"
	"time"

	"deeptalk/internal/infra/metrics"
	llmpkg "deeptalk/internal/llm"

	"github.com/cloudwego/eino/schema"
)

// messageCount 当前历史条数
func (a *AIHelper) messageCount() int {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return len(a.messages)
}

func usageOf(msg *schema.Message) *schema.TokenUsage {
	if msg == nil || msg.ResponseMeta == nil {
		return nil
	}
	return msg.ResponseMeta.Usage
}

// recordUsage 记录 token/费用/延迟，并累加到当日配额
func (a *AIHelper) recordUsage(userName, modelName, modelType string, usage *schema.TokenUsage, latency time.Duration, status, source string) {
	req := metrics.AIRequest{
		Model:     modelName,
		ModelType: modelType,
		User:      userName,
		Latency:   latency,
		Status:    status,
		Source:    source,
	}
	if usage != nil {
		req.PromptTokens = usage.PromptTokens
		req.CompletionTokens = usage.CompletionTokens
		req.CachedTokens = usage.PromptTokenDetails.CachedTokens
	}
	req.CostMicros = metrics.CostMicros(modelName, req.PromptTokens, req.CompletionTokens, req.CachedTokens)
	metrics.RecordAIRequest(req)

	if usage != nil {
		if ok, used, limit := metrics.CheckQuota(userName, usage.TotalTokens); !ok {
			log.Printf("[AIHelper] user=%s 超出每日配额: used=%d limit=%d", userName, used, limit)
		}
	}

	log.Printf("[AIHelper] model=%s user=%s status=%s source=%s prompt=%d completion=%d cached=%d cost=%.6f元 latency=%s",
		modelName, userName, status, source, req.PromptTokens, req.CompletionTokens, req.CachedTokens,
		float64(req.CostMicros)/1e6, latency)
}

func recordAIFailure(userName, modelName, modelType string, start time.Time, err error) {
	metrics.RecordAIRequest(metrics.AIRequest{
		Model:     modelName,
		ModelType: modelType,
		User:      userName,
		Latency:   time.Since(start),
		Status:    "error",
		Source:    "llm",
	})
	log.Printf("[AIHelper] model=%s user=%s 调用失败: %v", modelName, userName, err)
}

// streamCachedText 把缓存答案按小块吐出，保持与真实流式一致的观感
func streamCachedText(answer string, cb llmpkg.StreamCallback) {
	runes := []rune(answer)
	const chunk = 24
	for i := 0; i < len(runes); i += chunk {
		end := i + chunk
		if end > len(runes) {
			end = len(runes)
		}
		cb(string(runes[i:end]))
	}
}
