package chat

import (
	"context"
	"time"

	cachepkg "deeptalk/internal/cache"
	"deeptalk/internal/infra/metrics"
	"deeptalk/internal/infra/resilience"
	llmpkg "deeptalk/internal/llm"
	"deeptalk/model"
	"deeptalk/utils"

	"github.com/cloudwego/eino/schema"
)

// 同步生成
func (a *AIHelper) GenerateResponse(ctx context.Context, userName string, userQuestion string) (*model.Message, error) {
	// 同一会话同一时刻只处理一轮对话
	a.turn.Lock()
	defer a.turn.Unlock()
	a.touch()

	//调用存储函数
	a.AddMessage(userQuestion, userName, true, true)

	// 记忆压缩检查（锁外调用 LLM）
	a.compressIfNeeded(ctx)

	modelName := a.model.GetModelName()
	modelType := a.GetModelType()
	modelKey := modelType + "|" + modelName
	start := time.Now()

	// 配额预检：超额直接拒绝，避免继续烧 token
	if ok, used, limit := metrics.CheckQuota(userName, 0); !ok {
		metrics.RecordAIRequest(metrics.AIRequest{Model: modelName, ModelType: modelType, User: userName,
			Status: "quota_exceeded", Source: "llm", Latency: time.Since(start)})
		return nil, metrics.QuotaExceeded(used, limit)
	}

	// 语义缓存：仅对"没有历史上下文"的首轮提问生效
	firstTurn := a.messageCount() <= 1
	if firstTurn {
		if answer, hit := cachepkg.GetSemanticCache().Lookup(ctx, modelKey, userQuestion); hit {
			metrics.RecordAIRequest(metrics.AIRequest{Model: modelName, ModelType: modelType, User: userName,
				Status: "ok", Source: "semantic_cache", Latency: time.Since(start)})
			a.AddMessage(answer, userName, false, true)
			return &model.Message{SessionID: a.SessionID, UserName: userName, Content: answer, IsUser: false}, nil
		}
	}

	//调用模型生成回复（走熔断：上游连续失败时快速失败，不再把请求堆到已挂的服务上）
	var schemaMsg *schema.Message
	err := resilience.DoErr(resilience.ModelKey(modelName), func() error {
		var e error
		schemaMsg, e = a.model.GenerateResponse(ctx, a.schemaMessages())
		return e
	})
	if err != nil {
		recordAIFailure(userName, modelName, modelType, start, err)
		return nil, err
	}

	// 记录用量与费用
	usage := usageOf(schemaMsg)
	a.recordUsage(userName, modelName, modelType, usage, time.Since(start), "ok", "llm")

	//将schema.Message转化成model.Message
	modelMsg := utils.ConvertToModelMessage(a.SessionID, userName, schemaMsg)

	//调用存储函数
	a.AddMessage(modelMsg.Content, userName, false, true)

	if firstTurn {
		cachepkg.GetSemanticCache().Store(ctx, modelKey, userQuestion, modelMsg.Content)
	}

	return modelMsg, nil
}

// 流式生成
func (a *AIHelper) StreamResponse(ctx context.Context, userName string, cb llmpkg.StreamCallback, userQuestion string) (*model.Message, error) {
	a.turn.Lock()
	defer a.turn.Unlock()
	a.touch()

	//调用存储函数
	a.AddMessage(userQuestion, userName, true, true)

	// 记忆压缩检查（锁外调用 LLM）
	a.compressIfNeeded(ctx)

	modelName := a.model.GetModelName()
	modelType := a.GetModelType()
	modelKey := modelType + "|" + modelName
	start := time.Now()

	if ok, used, limit := metrics.CheckQuota(userName, 0); !ok {
		metrics.RecordAIRequest(metrics.AIRequest{Model: modelName, ModelType: modelType, User: userName,
			Status: "quota_exceeded", Source: "llm"})
		return nil, metrics.QuotaExceeded(used, limit)
	}

	firstTurn := a.messageCount() <= 1
	if firstTurn {
		if answer, hit := cachepkg.GetSemanticCache().Lookup(ctx, modelKey, userQuestion); hit {
			metrics.RecordAIRequest(metrics.AIRequest{Model: modelName, ModelType: modelType, User: userName,
				Status: "ok", Source: "semantic_cache", Latency: time.Since(start)})
			// 缓存答案没有"打字机"过程，这里分批吐出，前端体验一致
			streamCachedText(answer, cb)
			a.AddMessage(answer, userName, false, true)
			return &model.Message{SessionID: a.SessionID, UserName: userName, Content: answer, IsUser: false}, nil
		}
	}

	var content string
	var usage *schema.TokenUsage
	err := resilience.DoErr(resilience.ModelKey(modelName), func() error {
		var e error
		content, usage, e = a.model.StreamResponse(ctx, a.schemaMessages(), cb)
		return e
	})
	if err != nil {
		recordAIFailure(userName, modelName, modelType, start, err)
		return nil, err
	}
	a.recordUsage(userName, modelName, modelType, usage, time.Since(start), "ok", "llm")

	//转化成model.Message
	modelMsg := &model.Message{
		SessionID: a.SessionID,
		UserName:  userName,
		Content:   content,
		IsUser:    false,
	}

	//调用存储函数
	a.AddMessage(modelMsg.Content, userName, false, true)

	if firstTurn {
		cachepkg.GetSemanticCache().Store(ctx, modelKey, userQuestion, content)
	}

	return modelMsg, nil
}
