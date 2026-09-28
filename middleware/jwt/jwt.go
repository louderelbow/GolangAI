package jwt

import (
	"deeptalk/common/code"
	"deeptalk/controller"
	"deeptalk/utils/myjwt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// 读取jwt
func Auth() gin.HandlerFunc {
	return func(c *gin.Context) {
		res := new(controller.Response)

		var token string
		authHeader := c.GetHeader("Authorization")
		if authHeader != "" && strings.HasPrefix(authHeader, "Bearer ") {
			token = strings.TrimPrefix(authHeader, "Bearer ")
		} else {
			// 兼容 URL 参数传 token
			token = c.Query("token")
		}

		if token == "" {
			c.JSON(http.StatusOK, res.CodeOf(code.CodeInvalidToken))
			c.Abort()
			return
		}

		// 不要把完整 token 写进日志（日志会被收集/转发，等同于泄露凭据）。
		// 这里原本每个请求打一行 tokenLen，属于纯噪音——26 RPS 下每秒几十行写控制台，
		// 排障时用不上，已去掉。
		userName, ok := myjwt.ParseToken(token)
		if !ok {
			c.JSON(http.StatusOK, res.CodeOf(code.CodeInvalidToken))
			c.Abort()
			return
		}

		c.Set("userName", userName)
		c.Next()
	}
}
