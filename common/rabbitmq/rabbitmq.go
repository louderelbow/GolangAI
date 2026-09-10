package rabbitmq

import (
	"deeptalk/config"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/streadway/amqp"
)

const (
	dialTimeout       = 5 * time.Second  // 建连超时
	dialFailCooldown  = 5 * time.Second  // 建连失败后的冷却：避免每条消息都去等一次超时
	reconnectMaxDelay = 30 * time.Second // 消费者重连的最大退避
)

var (
	connMu         sync.Mutex
	conn           *amqp.Connection
	lastDialErr    error
	lastDialTry    time.Time
)

// dial 建立连接（带超时：amqp.Dial 默认没有超时，网络异常时会挂很久）
func dial() (*amqp.Connection, error) {
	c := config.GetConfig()
	mqUrl := fmt.Sprintf(
		"amqp://%s:%s@%s:%d/%s",
		c.RabbitmqUsername, c.RabbitmqPassword, c.RabbitmqHost, c.RabbitmqPort, c.RabbitmqVhost,
	)
	return amqp.DialConfig(mqUrl, amqp.Config{
		Dial: amqp.DefaultDial(dialTimeout),
	})
}

// ensureConn 获取可用连接；失败后短时间内直接返回缓存的错误（冷却）
func ensureConn() (*amqp.Connection, error) {
	connMu.Lock()
	defer connMu.Unlock()

	if conn != nil && !conn.IsClosed() {
		return conn, nil
	}

	if lastDialErr != nil && time.Since(lastDialTry) < dialFailCooldown {
		return nil, lastDialErr
	}
	lastDialTry = time.Now()

	newConn, err := dial()
	if err != nil {
		lastDialErr = err
		log.Printf("[RabbitMQ] connection failed: %v (will retry in %s)", err, dialFailCooldown)
		return nil, err
	}

	conn = newConn
	lastDialErr = nil
	log.Println("[RabbitMQ] connection established")
	return conn, nil
}

// invalidateConn 标记连接失效，下次自动重连
func invalidateConn(target *amqp.Connection) {
	connMu.Lock()
	defer connMu.Unlock()
	if conn == target {
		conn = nil
	}
}

// RabbitMQ RabbitMQ结构体
type RabbitMQ struct {
	mu       sync.Mutex
	channel  *amqp.Channel
	closed   chan *amqp.Error // 当前 channel 的关闭通知（用于判断是否需要重建）
	Exchange string
	Key      string
}

// NewRabbitMQ 创建RabbitMQ对象（连接是懒建立的，不可用时会自动重连）
func NewRabbitMQ(exchange string, key string) *RabbitMQ {
	return &RabbitMQ{Exchange: exchange, Key: key}
}

// NewWorkRabbitMQ 创建Work模式的RabbitMQ实例
func NewWorkRabbitMQ(queue string) *RabbitMQ {
	return NewRabbitMQ("", queue)
}

// ensureChannel 拿到可用的 channel，并按需（重新）声明队列
func (r *RabbitMQ) ensureChannel() (*amqp.Channel, chan *amqp.Error, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// streadway/amqp 的 Channel 没有 IsClosed()，用 NotifyClose 的通知通道判断
	if r.channel != nil {
		select {
		case <-r.closed:
			// 已关闭，下面重建
			_ = r.channel.Close()
			r.channel = nil
			r.closed = nil
		default:
			return r.channel, r.closed, nil
		}
	}

	c, err := ensureConn()
	if err != nil {
		return nil, nil, err
	}

	ch, err := c.Channel()
	if err != nil {
		invalidateConn(c)
		return nil, nil, fmt.Errorf("create channel failed: %w", err)
	}

	// 队列声明在建立 channel 时做一次即可，不必每条消息都做（原来每次 Publish 都声明）
	if _, err := ch.QueueDeclare(r.Key, true, false, false, false, nil); err != nil {
		_ = ch.Close()
		invalidateConn(c)
		return nil, nil, fmt.Errorf("declare queue failed: %w", err)
	}

	closed := make(chan *amqp.Error, 1)
	ch.NotifyClose(closed)

	r.channel = ch
	r.closed = closed
	return ch, closed, nil
}

// dropChannel 丢弃当前 channel，让下次调用重连
func (r *RabbitMQ) dropChannel(target *amqp.Channel) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.channel == target {
		_ = r.channel.Close()
		r.channel = nil
		r.closed = nil
	}
}

// Destroy 断开 channel 和 connection
func (r *RabbitMQ) Destroy() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.channel != nil {
		_ = r.channel.Close()
		r.channel = nil
	}
	connMu.Lock()
	defer connMu.Unlock()
	if conn != nil {
		_ = conn.Close()
		conn = nil
	}
}

// Publish 发送消息（持久化投递：队列 durable + 消息 persistent，broker 重启不丢）
func (r *RabbitMQ) Publish(message []byte) error {
	ch, _, err := r.ensureChannel()
	if err != nil {
		return err
	}

	err = ch.Publish(r.Exchange, r.Key, false, false,
		amqp.Publishing{
			ContentType:  "text/plain",
			DeliveryMode: amqp.Persistent,
			Body:         message,
		},
	)
	if err != nil {
		// 连接/信道已坏：丢弃 channel，下次自动重建
		r.dropChannel(ch)
		return err
	}
	return nil
}

// Consume 消费者（自带重连：MQ 重启后会自动恢复消费，不会永久丢消息）
// handle: 消息的消费业务函数，用于消费消息
func (r *RabbitMQ) Consume(handle func(msg *amqp.Delivery) error) {
	delay := time.Second
	for {
		err := r.consumeOnce(handle)
		if err == nil {
			return
		}
		log.Printf("[RabbitMQ] consumer stopped: %v, reconnecting in %s", err, delay)
		time.Sleep(delay)
		if delay < reconnectMaxDelay {
			delay *= 2
		}
	}
}

func (r *RabbitMQ) consumeOnce(handle func(msg *amqp.Delivery) error) error {
	ch, closeCh, err := r.ensureChannel()
	if err != nil {
		return err
	}

	// 接收消息 — autoAck=false，手动确认
	msgs, err := ch.Consume(r.Key, "", false, false, false, false, nil)
	if err != nil {
		r.dropChannel(ch)
		return err
	}

	log.Printf("[RabbitMQ] consumer started on queue=%s", r.Key)

	for {
		select {
		case msg, ok := <-msgs:
			if !ok {
				r.dropChannel(ch)
				return fmt.Errorf("delivery channel closed")
			}
			if err := handle(&msg); err != nil {
				log.Printf("[RabbitMQ] handle message failed: %v", err)
				if msg.Redelivered {
					// 已经重投过一次还是失败 → 判定为毒消息，直接丢弃并告警，
					// 否则 Requeue 会变成无限热循环，把 CPU 和日志打满
					log.Printf("[RabbitMQ] drop poison message: %s", string(msg.Body))
					_ = msg.Nack(false, false)
				} else {
					_ = msg.Nack(false, true)
				}
				continue
			}
			_ = msg.Ack(false)

		case closeErr := <-closeCh:
			r.dropChannel(ch)
			return fmt.Errorf("channel closed: %v", closeErr)
		}
	}
}
