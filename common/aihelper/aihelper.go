package aihelper

import (
	"context"
	"deeptalk/common/metrics"
	"deeptalk/common/rabbitmq"
	"deeptalk/model"
	"deeptalk/utils"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cloudwego/eino/schema"
)

// AIHelper AI助手结构体，包含消息历史和AI模型
type AIHelper struct {
	model     AIModel
	modelType string // 该会话绑定的模型类型（创建后不再改变）
	messages  []*model.Message
	summary   string       // 被压缩掉的早期历史的摘要，作为 system 上下文注入
	mu        sync.RWMutex // 保护 messages / summary
	turn      sync.Mutex   // 同一会话内的对话轮次串行化（避免并发写入导致历史错乱）
	// lastUsed 最近一次使用时间（纳秒），供管理器的空闲淘汰使用
	lastUsed int64
	//一个会话绑定一个AIHelper
	SessionID string
	saveFunc  func(*model.Message) (*model.Message, error)
}

// NewAIHelper 创建新的AIHelper实例
func NewAIHelper(model_ AIModel, SessionID string) *AIHelper {
	modelType := ""
	if model_ != nil {
		modelType = model_.GetModelType()
	}
	return &AIHelper{
		model:     model_,
		modelType: modelType,
		messages:  make([]*model.Message, 0),
		//异步推送到消息队列中（MQ 不可用时自动降级为同步写库，保证消息不丢）
		saveFunc: func(msg *model.Message) (*model.Message, error) {
			data := rabbitmq.GenerateMessageMQParam(msg.SessionID, msg.Content, msg.UserName, msg.IsUser)
			if err := rabbitmq.PublishOrPersist(data); err != nil {
				log.Printf("[AIHelper] persist message failed: %v", err)
			}
			return msg, nil
		},
		SessionID: SessionID,
	}
}

// GetModelType 返回该会话绑定的模型类型
func (a *AIHelper) GetModelType() string {
	if a.modelType != "" {
		return a.modelType
	}
	if a.model != nil {
		return a.model.GetModelType()
	}
	return ""
}

// touch 记录本次使用时间（供管理器做空闲淘汰）
func (a *AIHelper) touch() {
	atomic.StoreInt64(&a.lastUsed, time.Now().UnixNano())
}

// lastUsedNano 最近使用时间（纳秒）
func (a *AIHelper) lastUsedNano() int64 {
	return atomic.LoadInt64(&a.lastUsed)
}

// LoadHistory 用数据库中的历史初始化内存（仅用于会话首次加载时，不写 MQ）
func (a *AIHelper) LoadHistory(msgs []*model.Message) {
	if len(msgs) == 0 {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.messages = make([]*model.Message, 0, len(msgs))
	a.messages = append(a.messages, msgs...)
}

// AddMessage 添加消息到内存中并调用自定义存储函数（并发安全）
func (a *AIHelper) AddMessage(Content string, UserName string, IsUser bool, Save bool) {
	userMsg := model.Message{
		SessionID: a.SessionID,
		Content:   Content,
		UserName:  UserName,
		IsUser:    IsUser,
	}

	a.mu.Lock()
	a.messages = append(a.messages, &userMsg)
	a.mu.Unlock()

	if Save {
		a.saveFunc(&userMsg)
	}
}

// GetMessages 获取所有消息历史
func (a *AIHelper) GetMessages() []*model.Message {
	a.touch()
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make([]*model.Message, len(a.messages))
	copy(out, a.messages)
	return out
}

// snapshot 返回消息历史副本 + 当前摘要（读锁内完成）
func (a *AIHelper) snapshot() ([]*model.Message, string) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make([]*model.Message, len(a.messages))
	copy(out, a.messages)
	return out, a.summary
}

// schemaMessages 组装发送给模型的完整消息（含摘要 system 上下文）
func (a *AIHelper) schemaMessages() []*schema.Message {
	msgs, summary := a.snapshot()
	return utils.ConvertToSchemaMessages(msgs, summary)
}

// compressTokenLimit 记忆压缩的 token 预算，默认读取配置；抽成变量便于测试替换
var compressTokenLimit = GetMaxTokens

// compressIfNeeded 按 token 预算压缩历史（LLM 调用在锁外执行，不阻塞其他请求）
func (a *AIHelper) compressIfNeeded(ctx context.Context) {
	msgs, _ := a.snapshot()
	compressor := NewCompressor(compressTokenLimit())
	if !compressor.ShouldCompress(msgs) {
		return
	}

	recentMsgs, summary, err := compressor.Compress(ctx, msgs, a.model)
	if err != nil || summary == "" {
		return
	}

	a.mu.Lock()
	a.messages = recentMsgs
	a.summary = summary
	a.mu.Unlock()

	log.Printf("[AIHelper] memory compressed, session=%s kept=%d history, summary len=%d",
		a.SessionID, len(recentMsgs), len([]rune(summary)))
}

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
		if answer, hit := GetSemanticCache().Lookup(ctx, modelKey, userQuestion); hit {
			metrics.RecordAIRequest(metrics.AIRequest{Model: modelName, ModelType: modelType, User: userName,
				Status: "ok", Source: "semantic_cache", Latency: time.Since(start)})
			a.AddMessage(answer, userName, false, true)
			return &model.Message{SessionID: a.SessionID, UserName: userName, Content: answer, IsUser: false}, nil
		}
	}

	//调用模型生成回复
	schemaMsg, err := a.model.GenerateResponse(ctx, a.schemaMessages())
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
		GetSemanticCache().Store(ctx, modelKey, userQuestion, modelMsg.Content)
	}

	return modelMsg, nil
}

// 流式生成
func (a *AIHelper) StreamResponse(ctx context.Context, userName string, cb StreamCallback, userQuestion string) (*model.Message, error) {
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
		if answer, hit := GetSemanticCache().Lookup(ctx, modelKey, userQuestion); hit {
			metrics.RecordAIRequest(metrics.AIRequest{Model: modelName, ModelType: modelType, User: userName,
				Status: "ok", Source: "semantic_cache", Latency: time.Since(start)})
			// 缓存答案没有"打字机"过程，这里分批吐出，前端体验一致
			streamCachedText(answer, cb)
			a.AddMessage(answer, userName, false, true)
			return &model.Message{SessionID: a.SessionID, UserName: userName, Content: answer, IsUser: false}, nil
		}
	}

	content, usage, err := a.model.StreamResponse(ctx, a.schemaMessages(), cb)
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
		GetSemanticCache().Store(ctx, modelKey, userQuestion, content)
	}

	return modelMsg, nil
}

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
func streamCachedText(answer string, cb StreamCallback) {
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
