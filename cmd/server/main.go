package main

import (
	"context"
	"deeptalk/dao/message"
	daotrace "deeptalk/dao/trace"
	"deeptalk/internal/agent/trace"
	"deeptalk/internal/inference"
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

	// 注入轨迹落库：Prometheus 那一半在 trace.Flush 里已经出去了，
	// 这里补上"按 trace_id 回放单轮"的那一半。
	trace.SetStore(daotrace.Create)

	rabbitmq.InitRabbitMQ()
	log.Println("rabbitmq init success  ")

	// 提前初始化推理调度器：池子与指标都在这一步建立。
	// 不能等到第一个会话创建时才惰性初始化——那样启动后 /metrics 里
	// 看不到 deeptalk_inference_*，也没法在接流量前确认调度参数。
	infra := inference.Shared()
	log.Printf("[inference] scheduler ready: enabled=%v", infra.Enabled())

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

	// 先让调度器停止接收新的推理请求（排队中的立刻被拒），
	// 再等 HTTP 层把在途请求跑完——顺序反了的话，关闭期间还会有新请求排进队列。
	infra.Close()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("[main] graceful shutdown failed: %v", err)
	}

	// 兜底：HTTP 层已退出，正常在途推理应该都结束了
	if !infra.Drain(10 * time.Second) {
		log.Println("[main] 警告：仍有推理请求未在超时内结束")
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

	// Unified Agent 的上游可能和 RAG 不同，且 key 通常来自环境变量——
	// 这两件事都只在这里能一眼看清，出问题（401 / 调错上游）时全靠这一行。
	agentModel, agentBase, agentKey := conf.AgentModel()
	keyState := "未配置"
	if agentKey != "" {
		keyState = "已配置"
	}
	log.Printf("[config] Unified Agent 模型: %s @ %s（API Key %s）",
		agentModel, safeHost(agentBase), keyState)

	// 本地工作区默认是关的。运行时最容易被困惑的一点就是"我明明配了，
	// 怎么工具列表里没有 list_files / read_file"，所以这里明确打出来。
	if conf.GetLocalAgent().Enabled {
		log.Printf("[config] 本地工作区: 已启用（等待 deeptalk-agent 连接；未连接时这两个工具会回「未连接」）")
	} else {
		log.Printf("[config] 本地工作区: 未启用 —— [localAgent] enabled = false，Agent 不会注册 list_files / read_file")
	}

	log.Printf("[config] 语义缓存: enabled=%v 意图 LLM 兜底: %v 熔断: disabled=%v",
		conf.SemanticCache.Enabled, conf.IntentConfig.LLMFallback, conf.GetResilience().Disabled)
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
