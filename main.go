package main

import (
	"context"
	"deeptalk/common/aihelper"
	"deeptalk/common/metrics"
	"deeptalk/common/mysql"
	"deeptalk/common/rabbitmq"
	"deeptalk/common/redis"
	"deeptalk/config"
	"deeptalk/dao/message"
	"deeptalk/model"
	"deeptalk/router"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// loadSessionHistory 会话历史按需加载：
// 会话第一次进入内存时，把它自己的历史从数据库读回来（替代原来"启动时全表加载"）。
func loadSessionHistory(sessionID string) ([]*model.Message, error) {
	msgs, err := message.GetMessagesBySessionID(sessionID)
	if err != nil {
		return nil, err
	}
	out := make([]*model.Message, 0, len(msgs))
	for i := range msgs {
		out = append(out, &msgs[i])
	}
	return out, nil
}

func main() {
	conf := config.GetConfig()
	host := conf.MainConfig.Host
	port := conf.MainConfig.Port

	//初始化mysql
	if err := mysql.InitMysql(); err != nil {
		log.Println("InitMysql error , " + err.Error())
		return
	}

	// 注入历史加载器：内存里没有的会话按需从数据库恢复（懒加载，避免启动 OOM）
	aihelper.SetHistoryLoader(loadSessionHistory)
	// 空闲会话定期释放：否则 helper 会随"用户数 × 会话数"无限增长
	aihelper.GetGlobalManager().StartJanitor(aihelper.DefaultIdleTTL, aihelper.DefaultJanitorInterval)

	//初始化redis
	redis.Init()
	log.Println("redis init success  ")

	// 配额计数走 Redis（多实例共享、重启不丢）；Redis 不可用时自动退回进程内计数
	metrics.SetQuotaStore(redis.QuotaIncr)

	rabbitmq.InitRabbitMQ()
	log.Println("rabbitmq init success  ")

	srv := &http.Server{
		Addr:              fmt.Sprintf("%s:%d", host, port),
		Handler:           router.InitRouter(),
		ReadHeaderTimeout: 10 * time.Second, // 防止慢速 header 攻击
	}

	go func() {
		log.Printf("HTTP server listening on %s", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("[main] server error: %v", err)
		}
	}()

	// 优雅停机：收到信号后先停止接收新请求，等在途请求（含 SSE 流式回答）跑完
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("[main] shutting down, waiting for in-flight requests ...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("[main] graceful shutdown failed: %v", err)
	}
	log.Println("[main] server exited")
}
