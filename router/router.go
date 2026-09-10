package router

import (
	"deeptalk/common/logger"
	"deeptalk/common/metrics"
	"deeptalk/middleware/jwt"
	mw "deeptalk/middleware"

	"github.com/gin-gonic/gin"
)

func InitRouter() *gin.Engine {
	logger.Init("info")
	metrics.RegisterHelp()

	r := gin.Default()
	r.Use(mw.RequestID())

	// 指标端点（Prometheus 文本格式）：只暴露聚合计数，不含用户数据
	r.GET("/metrics", gin.WrapF(metrics.Handler()))

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
		ImageGroup := enterRouter.Group("/image")
		ImageGroup.Use(jwt.Auth())
		ImageGroup.Use(mw.BodyLimit(mw.MaxUploadBodyBytes))
		ImageRouter(ImageGroup)
	}

	{
		FileGroup := enterRouter.Group("/file")
		FileGroup.Use(jwt.Auth())
		FileGroup.Use(mw.BodyLimit(mw.MaxUploadBodyBytes))
		FileRouter(FileGroup)
	}

	return r
}
