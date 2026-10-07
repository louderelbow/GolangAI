package router

import (
	"deeptalk/controller/localagent"
	"deeptalk/controller/trace"
	"deeptalk/internal/infra/logger"
	"deeptalk/internal/infra/metrics"
	mw "deeptalk/middleware"
	"deeptalk/middleware/jwt"

	"github.com/gin-gonic/gin"
)

func InitRouter() *gin.Engine {
	logger.Init("info")
	metrics.RegisterHelp()

	// gin 的访问日志也走异步输出：必须在 gin.Default() 之前设置
	// （gin.Default() 里的 Logger 中间件会在创建时捕获 DefaultWriter）
	gin.DefaultWriter = logger.Output()

	r := gin.Default()
	r.Use(mw.RequestID())

	// 指标端点：由官方 promhttp 渲染（含 go_* / process_* 运行时指标）
	r.GET("/metrics", gin.WrapH(metrics.Handler()))

	enterRouter := r.Group("/api/v1")
	{
		// 未登录接口：按 IP 限流 + 限制 body 大小（防刷验证码/暴力破解）
		userGroup := enterRouter.Group("/user")
		userGroup.Use(mw.BodyLimit(mw.MaxAuthBodyBytes))
		userGroup.Use(mw.RateLimitByIP())
		RegisterUserRouter(userGroup)
	}
	//后续登录的接口需要jwt鉴权
	{
		AIGroup := enterRouter.Group("/AI")
		AIGroup.Use(jwt.Auth())
		AIGroup.Use(mw.BodyLimit(mw.MaxJSONBodyBytes))
		AIGroup.Use(mw.RateLimit())
		AIRouter(AIGroup)
	}

	{
		FileGroup := enterRouter.Group("/file")
		FileGroup.Use(jwt.Auth())
		FileGroup.Use(mw.BodyLimit(mw.MaxUploadBodyBytes))
		FileRouter(FileGroup)
	}

	{
		// 本地 agent：一条 WebSocket 长连接，身份完全靠 JWT
		// （原生客户端可以自己带 Authorization 头，没有同源限制）。
		//
		// 刻意不挂 BodyLimit / RateLimit：这是一条长驻连接而不是"请求"，
		// 按请求计数的限流会把它误伤成反复重连。
		LocalGroup := enterRouter.Group("/agent")
		LocalGroup.Use(jwt.Auth())
		LocalGroup.GET("/ws", localagent.Handle)
		LocalGroup.GET("/status", localagent.Status)
		LocalGroup.POST("/workspace", localagent.SetWorkspace)
		LocalGroup.GET("/online", localagent.Online)
	}

	{
		// 轨迹查询：只读、量小，跟着 AI 组一起限流即可。
		TraceGroup := enterRouter.Group("/AI/trace")
		TraceGroup.Use(jwt.Auth())
		TraceGroup.Use(mw.RateLimit())
		TraceGroup.GET("", trace.List)
		TraceGroup.GET("/:id", trace.Get)
	}

	return r
}
