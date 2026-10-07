package chat

import (
	"context"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"deeptalk/internal/agent/askuser"
	agentmemory "deeptalk/internal/agent/memory"
	"deeptalk/internal/infra/rabbitmq"
	llmpkg "deeptalk/internal/llm"
	"deeptalk/model"
	"deeptalk/utils"

	"github.com/cloudwego/eino/schema"
)

// AIHelper AI助手结构体，包含消息历史和AI模型
type AIHelper struct {
	model     llmpkg.AIModel
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

	// clarify 本轮 Agent 抛出的澄清提问（没有则为 nil）。
	// 由收尾逻辑写入、由上层在一轮结束后立刻取走：
	// 留着不清会让下一次普通回答被误判成"又在提问"。
	clarify *askuser.Request
}

// NewAIHelper 创建新的AIHelper实例
func NewAIHelper(model_ llmpkg.AIModel, SessionID string) *AIHelper {
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

// setClarify 记录本轮 Agent 抛出的澄清提问。
func (a *AIHelper) setClarify(req askuser.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.clarify = &req
}

// TakeClarify 取出并清空本轮的澄清提问（没有则返回 nil）。
//
// 由上层在一轮结束后立刻调用。必须"取走"而不是"读取"：
// 否则下一次普通回答会被误判成又需要澄清。
func (a *AIHelper) TakeClarify() *askuser.Request {
	a.mu.Lock()
	defer a.mu.Unlock()
	req := a.clarify
	a.clarify = nil
	return req
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
var compressTokenLimit = agentmemory.GetMaxTokens

// compressIfNeeded 按 token 预算压缩历史（LLM 调用在锁外执行，不阻塞其他请求）
func (a *AIHelper) compressIfNeeded(ctx context.Context) {
	msgs, _ := a.snapshot()
	compressor := agentmemory.NewCompressor(compressTokenLimit())
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
