package middleware

import (
	"context"
	"deeptalk/common/logger"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// RequestID 为每个请求注入唯一 ID，用于链路追踪
func RequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		// 优先使用客户端传来的，否则生成新的
		requestID := c.GetHeader("X-Request-ID")
		if requestID == "" {
			requestID = uuid.New().String()[:8] // 取前 8 位，够短够唯一
		}
		c.Set("requestId", requestID)
		c.Header("X-Request-ID", requestID)

		// 同时注入到 context.Context，供 service/dao 层使用
		ctx := context.WithValue(c.Request.Context(), logger.RequestIDKey, requestID)
		c.Request = c.Request.WithContext(ctx)

		c.Next()
	}
}
