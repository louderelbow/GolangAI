package aihelper

import (
	"context"
	"deeptalk/common/rabbitmq"
	"deeptalk/model"
	"deeptalk/utils"
	"log"
	"sync"

	"github.com/cloudwego/eino/schema"
)

// AIHelper AI助手结构体，包含消息历史和AI模型
type AIHelper struct {
	model     AIModel
	modelType string // 该会话绑定的模型类型（创建后不再改变）
	messages  []*model.Message
	summary   string // 被压缩掉的早期历史的摘要，作为 system 上下文注入

	mu   sync.RWMutex // 保护 messages / summary
	turn sync.Mutex   // 同一会话内的对话轮次串行化（避免并发写入导致历史错乱）

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
		//异步推送到消息队列中
		saveFunc: func(msg *model.Message) (*model.Message, error) {
			data := rabbitmq.GenerateMessageMQParam(msg.SessionID, msg.Content, msg.UserName, msg.IsUser)
			if rabbitmq.RMQMessage == nil {
				log.Printf("[AIHelper] RMQMessage is nil, skip publishing")
			} else if err := rabbitmq.RMQMessage.Publish(data); err != nil {
				log.Printf("[AIHelper] failed to publish message to MQ: %v", err)
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

	//调用存储函数
	a.AddMessage(userQuestion, userName, true, true)

	// 记忆压缩检查（锁外调用 LLM）
	a.compressIfNeeded(ctx)

	//调用模型生成回复
	schemaMsg, err := a.model.GenerateResponse(ctx, a.schemaMessages())
	if err != nil {
		return nil, err
	}

	//将schema.Message转化成model.Message
	modelMsg := utils.ConvertToModelMessage(a.SessionID, userName, schemaMsg)

	//调用存储函数
	a.AddMessage(modelMsg.Content, userName, false, true)

	return modelMsg, nil
}

// 流式生成
func (a *AIHelper) StreamResponse(ctx context.Context, userName string, cb StreamCallback, userQuestion string) (*model.Message, error) {
	a.turn.Lock()
	defer a.turn.Unlock()

	//调用存储函数
	a.AddMessage(userQuestion, userName, true, true)

	// 记忆压缩检查（锁外调用 LLM）
	a.compressIfNeeded(ctx)

	content, err := a.model.StreamResponse(ctx, a.schemaMessages(), cb)
	if err != nil {
		return nil, err
	}
	//转化成model.Message
	modelMsg := &model.Message{
		SessionID: a.SessionID,
		UserName:  userName,
		Content:   content,
		IsUser:    false,
	}

	//调用存储函数
	a.AddMessage(modelMsg.Content, userName, false, true)

	return modelMsg, nil
}
