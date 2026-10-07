package chat

import (
	"context"
	"time"

	cachepkg "deeptalk/internal/cache"
	"deeptalk/internal/infra/metrics"
	"deeptalk/internal/infra/resilience"
	llmpkg "deeptalk/internal/llm"
	"deeptalk/model"

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

	// 缓存只在"首轮提问"生效：有历史上下文时，同一个问题的正确答案会不同
	//（"它多少钱"在不同上下文里指代不同），命中必然答错。
	firstTurn := a.messageCount() <= 1

	// 回源：真正调用模型，并记录用量与费用
	generate := func() (cachepkg.Source, error) {
		var schemaMsg *schema.Message
		err := resilience.DoErr(resilience.ModelKey(modelName), func() error {
			var e error
			schemaMsg, e = a.model.GenerateResponse(ctx, a.schemaMessages())
			return e
		})
		if err != nil {
			return cachepkg.Source{}, err
		}
		usage := usageOf(schemaMsg)
		a.recordUsage(userName, modelName, modelType, usage, time.Since(start), "ok", "llm")
		return sourceOf(modelName, schemaMsg.Content, usage), nil
	}

	answer := ""
	level := cachepkg.LevelMiss
	if firstTurn {
		// 多级缓存 + 击穿防护：
		// 同一个 key 的并发回源只真正执行一次，其余请求共享结果——
		// 否则"热点 key 刚失效"会变成"模型被打 N 次"。
		hit, err := cachepkg.GetCoordinator().Do(ctx, modelKey, userQuestion, generate)
		if err != nil {
			recordAIFailure(userName, modelName, modelType, start, err)
			return nil, err
		}
		answer, level = hit.Answer, hit.Level
	} else {
		src, err := generate()
		if err != nil {
			recordAIFailure(userName, modelName, modelType, start, err)
			return nil, err
		}
		answer = src.Answer
	}

	if level != cachepkg.LevelMiss {
		// 命中缓存：这一次没有产生模型调用，来源标成命中的层级
		metrics.RecordAIRequest(metrics.AIRequest{Model: modelName, ModelType: modelType, User: userName,
			Status: "ok", Source: level, Latency: time.Since(start)})
	}

	//调用存储函数
	a.AddMessage(answer, userName, false, true)

	return &model.Message{SessionID: a.SessionID, UserName: userName, Content: answer, IsUser: false}, nil
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
		// 流式路径只用 Lookup：流是一段一段吐出来的，没法像同步路径那样
		// 用 singleflight 把并发请求合并成一次回源（会互相抢同一个流）。
		if hit := cachepkg.GetCoordinator().Lookup(ctx, modelKey, userQuestion); hit.OK {
			metrics.RecordAIRequest(metrics.AIRequest{Model: modelName, ModelType: modelType, User: userName,
				Status: "ok", Source: hit.Level, Latency: time.Since(start)})
			// 缓存答案没有"打字机"过程，这里分批吐出，前端体验一致
			streamCachedText(hit.Answer, cb)
			a.AddMessage(hit.Answer, userName, false, true)
			return &model.Message{SessionID: a.SessionID, UserName: userName, Content: hit.Answer, IsUser: false}, nil
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
		cachepkg.GetCoordinator().Store(ctx, modelKey, userQuestion, sourceOf(modelName, content, usage))
	}

	return modelMsg, nil
}
