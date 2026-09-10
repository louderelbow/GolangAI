package middleware

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// 请求体大小上限：防止超大 body 打爆内存（也顺带防止一次请求触发巨额模型 token 消耗）
const (
	MaxAuthBodyBytes   = 16 << 10  // 16KB  登录/注册/验证码
	MaxJSONBodyBytes   = 64 << 10  // 64KB  聊天等 JSON 接口
	MaxUploadBodyBytes = 10 << 20  // 10MB  图片识别/文档上传
)

// BodyLimit 限制单次请求体大小
// 超限时后续的 ShouldBindJSON / FormFile 会返回错误，由各 controller 转成参数错误
func BodyLimit(maxBytes int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.Body != nil {
			c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBytes)
		}
		c.Next()
	}
}
