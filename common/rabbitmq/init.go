package rabbitmq

import "log"

var (

	RMQMessage *RabbitMQ
)

func InitRabbitMQ() {
	// 创建MQ并启动消费者
	// 无论调用多少次 NewWorkRabbitMQ，只会创建一次连接
	// 不同队列共用一个连接，可以保持不同队列消费消息的顺序
	RMQMessage = NewWorkRabbitMQ("Message")

	// 启动时探测一次（连不上也继续启动，后续 Publish 会自动重连）
	if _, err := ensureConn(); err != nil {
		log.Println("[RabbitMQ] not available at startup, will retry lazily: " + err.Error())
	}

	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[RabbitMQ Consumer] panic recovered: %v", r)
			}
		}()
		// Consume 内部自带重连：MQ 重启后会自动恢复消费
		RMQMessage.Consume(MQMessage)
	}()
}
