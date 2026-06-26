package rabbitmq

import (
	"deeptalk/config"
	"fmt"
	"log"

	"github.com/streadway/amqp"
)

// 全局connection对象
// 所有RabbitMQ都会复用该对象
var conn *amqp.Connection

// 初始化connection
func initConn() {
	c := config.GetConfig()
	mqUrl := fmt.Sprintf(
		"amqp://%s:%s@%s:%d/%s",
		c.RabbitmqUsername, c.RabbitmqPassword, c.RabbitmqHost, c.RabbitmqPort, c.RabbitmqVhost,
	)
	log.Println("mqUrl is  " + mqUrl)
	var err error
	conn, err = amqp.Dial(mqUrl)
	if err != nil {
		log.Printf("RabbitMQ connection failed: %v, server will run without MQ", err)
	}
}

// RabbitMQ RabbitMQ结构体
type RabbitMQ struct {
	conn     *amqp.Connection
	channel  *amqp.Channel
	Exchange string
	Key      string
}

// NewRabbitMQ 创建RabbitMQ对象
func NewRabbitMQ(exchange string, key string) *RabbitMQ {
	return &RabbitMQ{Exchange: exchange, Key: key}
}

// Destroy 断开 channel 和 connection
func (r *RabbitMQ) Destroy() {
	_ = r.channel.Close()
	_ = r.conn.Close()
}

// NewWorkRabbitMQ 创建Work模式的RabbitMQ实例
func NewWorkRabbitMQ(queue string) *RabbitMQ {
	// new rabbitmq
	rabbitmq := NewRabbitMQ("", queue)

	// get connection
	if conn == nil {
		initConn()
	}
	if conn == nil {
		log.Println("[RabbitMQ] connection unavailable, NewWorkRabbitMQ returns nil")
		return nil
	}
	rabbitmq.conn = conn

	// get channel
	var err error
	rabbitmq.channel, err = rabbitmq.conn.Channel()
	if err != nil {
		log.Printf("[RabbitMQ] create channel failed: %v", err)
		return nil
	}

	return rabbitmq
}

// Publish 发送消息
func (r *RabbitMQ) Publish(message []byte) error {
	_, err := r.channel.QueueDeclare(r.Key, true, false, false, false, nil)
	if err != nil {
		return err
	}

	return r.channel.Publish(r.Exchange, r.Key, false, false,
		amqp.Publishing{
			ContentType: "text/plain",
			Body:        message,
		},
	)
}

// Consume 消费者
// handle: 消息的消费业务函数，用于消费消息
func (r *RabbitMQ) Consume(handle func(msg *amqp.Delivery) error) {
	q, err := r.channel.QueueDeclare(r.Key, true, false, false, false, nil)
	if err != nil {
		panic(err)
	}

	// 接收消息 — autoAck=false，手动确认
	msgs, err := r.channel.Consume(q.Name, "", false, false, false, false, nil)
	if err != nil {
		panic(err)
	}

	// 处理消息
	for msg := range msgs {
		if err := handle(&msg); err != nil {
			fmt.Println(err.Error())
			// 处理失败，重新入队
			_ = msg.Nack(false, true)
		} else {
			// 处理成功，确认消费
			_ = msg.Ack(false)
		}
	}
}
