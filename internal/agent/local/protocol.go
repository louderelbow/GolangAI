package local

import (
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// ======================== 线上协议 ========================
//
// 服务端与本地 agent 之间只说这几种帧。协议刻意做得很小：这条链路跨越了
// "你的服务器"和"用户的电脑"两个信任域，越小越好审计。
//
//	hello          本地 → 服务端   连接建立后第一帧，上报工作区与版本
//	call           服务端 → 本地   下发一次工具调用
//	result         本地 → 服务端   回投执行结果
//	set_workspace  服务端 → 本地   请求切换工作区（path 为空 = 让本机弹选择框）
//	workspace      本地 → 服务端   切换结果，也是"我现在的工作区是什么"的唯一真相
//
// 最后两种是后加的：工作区是**本地那端**的概念，服务端不能替用户决定，
// 但用户显然应该在网页里点一下就能换，而不是回终端改启动参数。
// 所以由网页发起 → 服务端转发 → 本机校验/弹框 → 回报结果。
const (
	FrameHello        = "hello"
	FrameCall         = "call"
	FrameResult       = "result"
	FrameSetWorkspace = "set_workspace"
	FrameWorkspace    = "workspace"
)

// Frame 是这条链路上的统一帧结构。
//
// 几种帧共用一个结构体而不是各定义一套：字段少且互斥，
// 拆成一堆类型只会让序列化代码比协议本身还长。
type Frame struct {
	Type string `json:"type"`

	// hello / workspace
	Workspace string `json:"workspace,omitempty"`
	Version   string `json:"version,omitempty"`
	// OK 只在 workspace 帧里有意义：本次切换是否成功。
	OK bool `json:"ok,omitempty"`

	// call / result / set_workspace
	ID     string `json:"id,omitempty"`
	Tool   string `json:"tool,omitempty"`
	Args   string `json:"args,omitempty"`
	Output string `json:"output,omitempty"`
	Error  string `json:"error,omitempty"`
}

// 连接参数。
//
// 三个值得注意的取值：
//   - maxFrameBytes 限制单帧大小，防止对端（或被篡改的对端）灌爆内存；
//     一个目录列表或一段文件内容远用不到 8MB。
//   - pongWait 是"读超时"：这么久没收到任何帧（含 pong）就判定连接已死。
//     不做这个检测的话，用户拔网线后服务端会一直以为他还连着，
//     工具调用会一直静静地堆在 pending 里直到超时。
//   - pingPeriod 必须显著小于 pongWait，否则正常网络抖动也会误判掉线。
const (
	maxFrameBytes = 8 << 20
	pongWait      = 60 * time.Second
	pingPeriod    = 25 * time.Second
	writeWait     = 10 * time.Second
)

// Conn 是对 websocket.Conn 的薄封装：统一分帧、心跳与"写必须串行"。
//
// 服务端与本地 agent 共用同一份实现，是为了避免两边对协议的理解各自漂移——
// 这类跨进程协议一旦有两份实现，最先坏掉的总是"只有一边记得的细节"。
type Conn struct {
	ws      *websocket.Conn
	writeMu sync.Mutex
}

// NewConn 接管一条已建立的 WebSocket 连接并装上心跳处理。
func NewConn(ws *websocket.Conn) *Conn {
	ws.SetReadLimit(maxFrameBytes)
	_ = ws.SetReadDeadline(time.Now().Add(pongWait))
	// 每收到一个 pong 就把读超时往后推：连接活着，超时就永远不到期
	ws.SetPongHandler(func(string) error {
		return ws.SetReadDeadline(time.Now().Add(pongWait))
	})
	return &Conn{ws: ws}
}

// Write 发送一帧。
//
// gorilla 不允许并发写同一连接（会 panic），而工具调用本来就可能并发
// （模型一轮里连着调两个），所以这里必须串行化。
func (c *Conn) Write(f Frame) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := c.ws.SetWriteDeadline(time.Now().Add(writeWait)); err != nil {
		return err
	}
	return c.ws.WriteJSON(f)
}

// Read 读取一帧；连接断开或读超时都会返回错误。
func (c *Conn) Read() (Frame, error) {
	var f Frame
	err := c.ws.ReadJSON(&f)
	return f, err
}

// PingLoop 周期性发 ping，直到 stop 关闭。
//
// 只负责"发"：对端回不回由 pong handler 与本端的读超时判定。
// WriteControl 按 gorilla 的约定可以与其它写并发，因此不必抢 writeMu。
func (c *Conn) PingLoop(stop <-chan struct{}) {
	t := time.NewTicker(pingPeriod)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			if err := c.ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(writeWait)); err != nil {
				return
			}
		}
	}
}

// Close 关闭底层连接。
func (c *Conn) Close() error { return c.ws.Close() }
