package rabbitmq

import (
	"deeptalk/dao/message"
	"deeptalk/model"
	"encoding/json"
	"log"

	"github.com/streadway/amqp"
)

type MessageMQParam struct {
	SessionID string `json:"session_id"`
	Content   string `json:"content"`
	UserName  string `json:"user_name"`
	IsUser    bool   `json:"is_user"`
}

func GenerateMessageMQParam(sessionID string, content string, userName string, IsUser bool) []byte {
	param := MessageMQParam{
		SessionID: sessionID,
		Content:   content,
		UserName:  userName,
		IsUser:    IsUser,
	}
	data, _ := json.Marshal(param)
	return data
}

// PublishOrPersist 优先走 MQ 异步落库；MQ 不可用或投递失败时退化为同步写库
// 这样"消息不丢"才是真的：不会因为 MQ 挂了/重启而丢掉聊天记录
func PublishOrPersist(data []byte) error {
	if RMQMessage != nil {
		if err := RMQMessage.Publish(data); err == nil {
			return nil
		} else {
			log.Printf("[RabbitMQ] publish failed, fallback to sync DB write: %v", err)
		}
	}

	var param MessageMQParam
	if err := json.Unmarshal(data, &param); err != nil {
		return err
	}
	return PersistMessage(&param)
}

// PersistMessage 直接把消息写入数据库（消费者与降级路径共用）
func PersistMessage(param *MessageMQParam) error {
	newMsg := &model.Message{
		SessionID: param.SessionID,
		Content:   param.Content,
		UserName:  param.UserName,
		IsUser:    param.IsUser,
	}
	if _, err := message.CreateMessage(newMsg); err != nil {
		log.Printf("[RabbitMQ] failed to create message: %v", err)
		return err
	}
	return nil
}

func MQMessage(msg *amqp.Delivery) error {
	var param MessageMQParam
	err := json.Unmarshal(msg.Body, &param)
	if err != nil {
		return err
	}
	//消费者异步插入到数据库中
	return PersistMessage(&param)
}
