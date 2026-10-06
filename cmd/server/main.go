package main

import (
	"context"
	"deeptalk/dao/message"
	"deeptalk/internal/infra/config"
	"deeptalk/internal/infra/diag"
	"deeptalk/internal/infra/logger"
	"deeptalk/internal/infra/metrics"
	"deeptalk/internal/infra/mysql"
	"deeptalk/internal/infra/rabbitmq"
	"deeptalk/internal/infra/redis"
	"deeptalk/internal/infra/resilience"
	"deeptalk/model"
	"deeptalk/router"
	"deeptalk/service/chat"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// loadSessionHistory 会话历史按需加载：
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
	// 日志先切成异步批量写：同步写 Windows 控制台是 WriteConsole 系统调用，
	// 压测 profile 里独占 13%+ CPU，而且会把请求 goroutine 卡在 I/O 上。
	logger.EnableAsyncOutput()
	tuneDefaultHTTPTransport()

	conf := config.GetConfig()
	host := conf.MainConfig.Host
	port := conf.MainConfig.Port

	logStartupBanner(conf)

	//初始化mysql
	if err := mysql.InitMysql(); err != nil {
		log.Println("InitMysql error , " + err.Error())
		return
	}

	// 注入历史加载器：内存里没有的会话按需从数据库恢复（懒加载，避免启动 OOM）
	chat.SetHistoryLoader(loadSessionHistory)
	// 空闲会话定期释放：否则 helper 会随"用户数 × 会话数"无限增长
	chat.GetGlobalManager().StartJanitor(chat.DefaultIdleTTL, chat.DefaultJanitorInterval)

	//初始化redis
	redis.Init()
	log.Println("redis init success  ")

	// 配额计数走 Redis（多实例共享、重启不丢），外面套熔断器：
	// Redis 连续失败会打开熔断，此时自动退回进程内计数，不再等连接超时
	metrics.SetQuotaStore(func(user, day string, delta int64) (int64, error) {
		return resilience.Do(resilience.RedisKey("quota"), func() (int64, error) {
			return redis.QuotaIncr(user, day, delta)
		})
	})

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

	// 诊断端点（压测 / 排查用）：默认关闭，打开后只监听本机
	pprofSrv := startPprofIfEnabled(conf)

	// 优雅停机：收到信号后先停止接收新请求，等在途请求（含 SSE 流式回答）跑完
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("[main] shutting down, waiting for in-flight requests ...")

	diag.ShutdownPprof(pprofSrv)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("[main] graceful shutdown failed: %v", err)
	}

	// 日志是异步批量写的，退出前必须 flush，否则最后一批日志会丢
	if n := logger.DroppedLogs(); n > 0 {
		log.Printf("[main] 警告：有 %d 条日志因队列满被丢弃（不影响业务）", n)
	}
	log.Println("[main] server exited")
	logger.CloseOutput(2 * time.Second)
}

// tuneDefaultHTTPTransport 调大默认 HTTP 连接池，消除"每请求重新建连"。
//
// 起因（压测 CPU profile）：net/http.(*Transport).dialConn 占了 14% 的 CPU，
// 也就是高并发下连接根本没被复用。根因是 Go 默认 Transport 的
// MaxIdleConnsPerHost 只有 2：20 个并发请求打同一个上游，只留 2 条空闲连接，
// 其余用完立刻关闭，下一个请求只能重新 TCP 握手。
//
// 我们依赖的几个 SDK（go-openai、volcengine arkruntime）都是 client 的 Transport
// 留空，因此用的就是 http.DefaultTransport，改它一处即可全局生效。
// 必须在开始处理请求之前调用——http.Transport 的字段不允许并发读写。
func tuneDefaultHTTPTransport() {
	t, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return
	}
	t.MaxIdleConns = 200
	t.MaxIdleConnsPerHost = 100
	t.IdleConnTimeout = 90 * time.Second
	t.ForceAttemptHTTP2 = true
	log.Printf("[config] HTTP 连接池: MaxIdleConnsPerHost=%d IdleConnTimeout=%s",
		t.MaxIdleConnsPerHost, t.IdleConnTimeout)
}

// startPprofIfEnabled 按配置启动 /debug/pprof（关闭时返回 nil）
func startPprofIfEnabled(conf *config.Config) *http.Server {
	dbg := conf.GetDebug()
	if !dbg.PprofEnabled {
		return nil
	}
	return diag.StartPprof(dbg.PprofAddr)
}

// logStartupBanner 启动时把"生效的是哪份配置、关键开关长什么样"打出来。
//
// 起因：压测时忘了在终端设 DEEPTALK_CONFIG，后端静默回落到 config/config.toml，
// 结果压测流量全打到真实模型 API 上。少一个环境变量没有任何提示，代价却很高，
// 所以启动就把这几条打出来，一眼能看出配置有没有生效。
// 注意只打印地址和开关，绝不打印任何 API Key。
func logStartupBanner(conf *config.Config) {
	log.Printf("[config] 配置文件: %s", config.ConfigFile())

	rl := conf.GetRateLimit()
	rlState := "开启"
	if !rl.IsEnabled() {
		rlState = "已关闭（! 仅压测时应如此）"
	}
	log.Printf("[config] 限流: %s（单用户 capacity=%.0f refill=%.0f/s）",
		rlState, float64(rl.Capacity), float64(rl.Refill))

	dbg := conf.GetDebug()
	log.Printf("[config] pprof: enabled=%v addr=%s", dbg.PprofEnabled, dbg.PprofAddr)

	log.Printf("[config] RAG 模型: %s @ %s（embedding=%s dim=%d）",
		conf.RagModelConfig.RagChatModelName, safeHost(conf.RagModelConfig.RagBaseUrl),
		conf.RagModelConfig.RagEmbeddingModel, conf.RagModelConfig.RagDimension)

	// DeepSeek 那一路的地址来自环境变量，压测时也要指向 mock，顺手打出来
	baseURL, modelName, _ := deepSeekBaseURL()
	log.Printf("[config] DeepSeek 模型: %s @ %s", modelName, safeHost(baseURL))

	log.Printf("[config] 语义缓存: enabled=%v 意图 LLM 兜底: %v 熔断: disabled=%v",
		conf.SemanticCache.Enabled, conf.IntentConfig.LLMFallback, conf.GetResilience().Disabled)
}

// deepSeekBaseURL 复刻 common/aihelper 里 DeepSeek 的地址解析优先级
// （那里是包内私有函数，这里只需要打印，不引入依赖）
func deepSeekBaseURL() (baseURL, modelName, apiKey string) {
	first := func(vals ...string) string {
		for _, v := range vals {
			if v != "" {
				return v
			}
		}
		return ""
	}
	baseURL = first(os.Getenv("DEEPSEEK_BASE_URL"), os.Getenv("OPENAI_BASE_URL"), "https://api.deepseek.com")
	modelName = first(os.Getenv("DEEPSEEK_MODEL_NAME"), os.Getenv("OPENAI_MODEL_NAME"), "deepseek-chat")
	apiKey = first(os.Getenv("DEEPSEEK_API_KEY"), os.Getenv("OPENAI_API_KEY"))
	return baseURL, modelName, apiKey
}

// safeHost 只保留 scheme://host[:port]，避免把 URL 里可能带的凭据打进日志
func safeHost(raw string) string {
	if raw == "" {
		return "(空)"
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	return u.Scheme + "://" + u.Host
}
