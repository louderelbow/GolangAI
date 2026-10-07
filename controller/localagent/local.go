// Package localagent 提供本地 agent 连上服务端的入口。
//
// 只有一条路由：一个 WebSocket。本地 agent 主动连进来，之后所有工具调用
// 都复用这条连接——服务端永远不主动拨号到用户机器。
package localagent

import (
	"context"
	"log"
	"net/http"
	"strings"
	"time"

	agentlocal "deeptalk/internal/agent/local"
	"deeptalk/internal/infra/config"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

// upgrader 只负责把 HTTP 升级成 WebSocket。
//
// CheckOrigin 必须自己实现：gorilla 的默认策略是"同源才放行"，
// 而本地 agent 是原生程序，压根不发 Origin 头，默认策略会把所有连接挡掉。
// 这条连接的身份完全由 JWT（路由上的 Auth 中间件）决定，不依赖 Origin。
var upgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin:     func(*http.Request) bool { return true },
}

// Handle 处理本地 agent 的 WebSocket 连接。
//
// 握手后必须先收到一帧 hello 才认这条连接：hello 里带着工作区——
// 那是本地那端的概念（模型后续所有路径都相对它解析），服务端只负责转述，
// 自己不参与路径解析，因此更不能凭空假设一个默认工作区。
func Handle(c *gin.Context) {
	user := strings.TrimSpace(c.GetString("userName"))
	if user == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "未认证"})
		return
	}

	ws, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		// Upgrade 失败时它已经自己写过响应了，这里只记日志
		log.Printf("[localagent] user=%s 升级失败: %v", user, err)
		return
	}

	conn := agentlocal.NewConn(ws)

	f, err := conn.Read()
	if err != nil || f.Type != agentlocal.FrameHello {
		log.Printf("[localagent] user=%s 首帧不是 hello（type=%q err=%v），拒绝该连接", user, f.Type, err)
		_ = conn.Close()
		return
	}

	// Attach 阻塞直到连接结束，和 gin handler 的生命周期一致。
	// 期间所有工具调用都通过它下发。
	agentlocal.GetRegistry().Attach(user, conn, f.Workspace, f.Version)
}

// Online 返回当前在线的本地 agent 数量（诊断用）。
func Online(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"online": agentlocal.GetRegistry().Online(),
	})
}

// Status 返回当前用户的本地 agent 连接状态，供前端展示。
//
// 前端靠它决定显示"未连接（附一次性的启动指引）"还是"已连接 · 工作区 X"。
// enabled 一并带出去：服务端把能力关掉时，界面要能说清"不是你没启动，
// 是服务端没开"，而不是让用户对着一个永远连不上的按钮发呆。
func Status(c *gin.Context) {
	enabled := config.GetConfig().GetLocalAgent().Enabled

	s, ok := agentlocal.GetRegistry().Current(c.GetString("userName"))
	if !ok {
		c.JSON(http.StatusOK, gin.H{"connected": false, "enabled": enabled})
		return
	}

	ws, version, connectedAt := s.Info()
	c.JSON(http.StatusOK, gin.H{
		"connected":   true,
		"enabled":     enabled,
		"workspace":   ws,
		"version":     version,
		"connectedAt": connectedAt.Format(time.RFC3339),
	})
}

// SetWorkspace 请求本地 agent 更换工作区。
//
// path 只是**选择框的初始位置**，最终由用户在自己电脑上确认——
// 这个决定权不能交给服务端，理由见 cmd/localagent 的 handleSetWorkspace。
//
// 超时给到 2 分钟：用户要在弹出的目录框里翻目录，几十秒很正常。
func SetWorkspace(c *gin.Context) {
	var req struct {
		Path string `json:"path"`
	}
	_ = c.ShouldBindJSON(&req)

	s, ok := agentlocal.GetRegistry().Current(c.GetString("userName"))
	if !ok {
		c.JSON(http.StatusOK, gin.H{"error": "本地 agent 未连接，请先在你的电脑上启动它"})
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Minute)
	defer cancel()

	ws, err := s.SetWorkspace(ctx, req.Path)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"connected": true, "workspace": ws})
}
