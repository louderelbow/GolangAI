package aihelper

import (
	"context"
	"deeptalk/common/rabbitmq"
	"deeptalk/model"
	"deeptalk/utils"
	"log"
	"sync"
)

// AIHelper AI助手结构体，包含消息历史和AI模型
type AIHelper struct {
	model    AIModel
	messages []*model.Message
	mu       sync.RWMutex
	//一个会话绑定一个AIHelper
	SessionID string
	saveFunc  func(*model.Message) (*model.Message, error)
}

// NewAIHelper 创建新的AIHelper实例
func NewAIHelper(model_ AIModel, SessionID string) *AIHelper {
	return &AIHelper{
		model:    model_,
		messages: make([]*model.Message, 0),
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

// addMessage 添加消息到内存中并调用自定义存储函数
func (a *AIHelper) AddMessage(Content string, UserName string, IsUser bool, Save bool) {
	userMsg := model.Message{
		SessionID: a.SessionID,
		Content:   Content,
		UserName:  UserName,
		IsUser:    IsUser,
	}
	a.messages = append(a.messages, &userMsg)
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

// 同步生成
func (a *AIHelper) GenerateResponse(userName string, ctx context.Context, userQuestion string) (*model.Message, error) {

	//调用存储函数
	a.AddMessage(userQuestion, userName, true, true)

	// 记忆压缩检查
	a.mu.Lock()
	compressor := NewCompressor(GetMaxTokens())
	if compressor.ShouldCompress(a.messages) {
		newMsgs, summary, err := compressor.Compress(a.messages, a.model)
		if err == nil && summary != "" {
			a.messages = newMsgs
			log.Printf("[AIHelper] memory compressed, session=%s tokens saved", a.SessionID)
		}
	}
	messages := utils.ConvertToSchemaMessages(a.messages)
	a.mu.Unlock()

	//调用模型生成回复
	schemaMsg, err := a.model.GenerateResponse(ctx, messages)
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
func (a *AIHelper) StreamResponse(userName string, ctx context.Context, cb StreamCallback, userQuestion string) (*model.Message, error) {

	//调用存储函数
	a.AddMessage(userQuestion, userName, true, true)

	// 记忆压缩检查
	a.mu.Lock()
	compressor := NewCompressor(GetMaxTokens())
	if compressor.ShouldCompress(a.messages) {
		newMsgs, summary, err := compressor.Compress(a.messages, a.model)
		if err == nil && summary != "" {
			a.messages = newMsgs
			log.Printf("[AIHelper] memory compressed, session=%s tokens saved", a.SessionID)
		}
	}
	messages := utils.ConvertToSchemaMessages(a.messages)
	a.mu.Unlock()

	content, err := a.model.StreamResponse(ctx, messages, cb)
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
