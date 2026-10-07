// Package local 管理与"本地 agent"之间的连接。
//
// 要解决的问题：模型需要读写**用户自己电脑**上的文件，但服务端进程碰不到
// 用户的硬盘。于是把"执行器"挪到用户机器上——本地 agent 主动连上来，
// 服务端把工具调用下发给它，它执行完把结果送回来。
//
// 为什么是本地主动连服务端，而不是反过来：
//   - 服务端可能部署在远端，根本 dial 不到用户的 127.0.0.1；
//   - 用户机器不需要开任何入站端口，防火墙 / NAT 都不用管。
//
// 传输用 WebSocket：这条链路是**双向、长驻、低频**的，正是它擅长的形态。
// （用长轮询也能做，但每来一次工具调用都要多一次完整的 HTTP 往返，
// 而且要在服务端维护"待领队列"，把本可以很直白的请求-响应绕成一圈。）
//
// 安全边界不在这里，而在本地那端：服务端只负责转发，真正决定能不能碰某个
// 文件的是本地 agent 的工作区校验。这个分工是刻意的——做决策的进程碰不到
// 硬盘，能碰硬盘的进程不持有模型密钥。
package local

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/google/uuid"
)

// 本地 agent 未连接时返回的错误。
//
// 文案是给**模型**看的（会被回填成 observation），所以写清楚"该怎么办"
// 而不是只说"失败了"——模型据此才能如实告诉用户，而不是反复重试。
var (
	ErrNotConnected = errors.New("本地 agent 未连接：用户还没在自己电脑上启动 deeptalk-agent，请告知用户先启动它再试")
	ErrAgentGone    = errors.New("本地 agent 在执行过程中断开了连接，本次操作的结果未知")
)

// reply 一次请求-响应的内部投递结果。
type reply struct {
	f Frame

	// cause 非空表示这是**本端生成**的中断（例如连接断开），而不是对端回投的。
	//
	// 为什么要单独留一个字段：对端回投的错误只能以字符串过来，
	// errors.New 之后用 errors.Is 就认不出来了。而"agent 断了"是调用方
	// 需要精确区分的语义（它意味着结果未知，而不是执行失败），
	// 所以这一种必须保住 identity。
	cause error
}

// Session 一个已连接的本地 agent。
type Session struct {
	User    string
	Version string

	Connected time.Time

	conn *Conn
	done chan struct{}

	mu        sync.Mutex
	workspace string
	pending   map[string]chan reply
	closed    bool
}

func newSession(user, workspace, version string, conn *Conn) *Session {
	return &Session{
		User:      user,
		workspace: workspace,
		Version:   version,
		Connected: time.Now(),
		conn:      conn,
		done:      make(chan struct{}),
		pending:   make(map[string]chan reply),
	}
}

// Info 返回会话当前状态，供状态接口与诊断使用。
//
// 工作区可能在会话中途被用户通过网页改掉，所以一律走这个带锁的访问器，
// 不要直接读字段。
func (s *Session) Info() (workspace, version string, connected time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.workspace, s.Version, s.Connected
}

// deliver 把一帧交回正在等待它的调用；返回 false 表示这个 ID 已经没人等了
// （调用方已超时放弃，或连接被顶替后清理过）。
func (s *Session) deliver(f Frame) bool {
	s.mu.Lock()
	ch, ok := s.pending[f.ID]
	if ok {
		delete(s.pending, f.ID)
	}
	s.mu.Unlock()

	if !ok {
		return false
	}
	ch <- reply{f: f}
	return true
}

// roundTrip 下发一帧并等待指定类型的回应。
//
// 工具调用与"切换工作区"走的是同一套：两者都是"服务端请求、本地执行、
// 回投结果"，差别只在帧类型。抽成一条路径是为了让超时、断开唤醒、
// 迟到丢弃这些语义只有一份实现——三个地方各写一遍必然有一个漏掉。
func (s *Session) roundTrip(ctx context.Context, req Frame, want string) (Frame, error) {
	id := uuid.NewString()
	req.ID = id
	ch := make(chan reply, 1)

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return Frame{}, ErrAgentGone
	}
	s.pending[id] = ch
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
	}()

	if err := s.conn.Write(req); err != nil {
		return Frame{}, err
	}

	select {
	case r := <-ch:
		if r.cause != nil {
			return Frame{}, r.cause
		}
		if r.f.Type != want {
			return Frame{}, fmt.Errorf("本地 agent 回了意外的帧类型 %q（期望 %q）", r.f.Type, want)
		}
		return r.f, nil
	case <-ctx.Done():
		return Frame{}, ctx.Err()
	}
}

// Invoke 把一次工具调用下发给本地 agent 并等待结果。
//
// ctx 由上层（工具超时）控制：超时后本函数返回，但本地那次执行可能仍在跑。
// 它的结果回来时因为 pending 里已经没有这个 ID 而被丢弃——这是刻意的，
// 迟到的结果没人能用；但**副作用可能已经发生**，所以将来加有副作用的工具时，
// 必须由本地那端做确认（见 cmd/localagent 的说明）。
func (s *Session) Invoke(ctx context.Context, tool, args string) (string, error) {
	f, err := s.roundTrip(ctx, Frame{Type: FrameCall, Tool: tool, Args: args}, FrameResult)
	if err != nil {
		return "", err
	}
	if f.Error != "" {
		return f.Output, errors.New(f.Error)
	}
	return f.Output, nil
}

// SetWorkspace 请求本地 agent 更换工作区，成功后会话记录随之更新。
//
// path 为空表示"让用户在自己的电脑上选"——那一刻本地 agent 会弹一个
// 系统目录选择框。这是刻意的：工作区是用户机器上的概念，最终决定权必须在
// 那台机器上，服务端只能提请。
func (s *Session) SetWorkspace(ctx context.Context, path string) (string, error) {
	f, err := s.roundTrip(ctx, Frame{Type: FrameSetWorkspace, Args: path}, FrameWorkspace)
	if err != nil {
		return "", err
	}
	if !f.OK {
		msg := f.Error
		if msg == "" {
			msg = "本地 agent 切换工作区失败"
		}
		return "", errors.New(msg)
	}

	s.mu.Lock()
	s.workspace = f.Workspace
	s.mu.Unlock()

	log.Printf("[localagent] user=%s 工作区 -> %s", s.User, f.Workspace)
	return f.Workspace, nil
}

// Close 结束会话，让所有还在等待的调用立刻拿到"agent 断了"。
//
// 必须唤醒而不是干等超时：用户合盖/断网时，让每个在途工具都白等满
// 十几秒超时没有任何意义。幂等，可以在多处调用。
func (s *Session) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	close(s.done)

	for id, ch := range s.pending {
		select {
		case ch <- reply{cause: ErrAgentGone}:
		default:
		}
		delete(s.pending, id)
	}
	s.mu.Unlock()

	_ = s.conn.Close()
}

// readLoop 收帧直到连接断开。
func (s *Session) readLoop() {
	for {
		f, err := s.conn.Read()
		if err != nil {
			log.Printf("[localagent] user=%s 连接结束: %v", s.User, err)
			return
		}
		switch f.Type {
		case FrameResult, FrameWorkspace:
			if !s.deliver(f) {
				// 多半是调用方已经超时。不是错误，但值得留痕——
				// 它意味着"用户电脑上确实执行了，只是我们没等到"。
				log.Printf("[localagent] user=%s 收到迟到/未知的 %s id=%s，已丢弃", s.User, f.Type, f.ID)
			}
		default:
			log.Printf("[localagent] user=%s 收到未知帧 type=%q，已忽略", s.User, f.Type)
		}
	}
}

// Registry 管理"用户 → 本地 agent 会话"的映射。
type Registry struct {
	mu       sync.RWMutex
	sessions map[string]*Session
}

var globalRegistry = &Registry{sessions: make(map[string]*Session)}

// NewRegistry 构造一个独立的注册表（测试用；进程内运行请用 GetRegistry）。
func NewRegistry() *Registry { return &Registry{sessions: make(map[string]*Session)} }

// GetRegistry 返回进程级共享的注册表。
//
// 做成单例是因为连接本身是进程级资源，而工具与控制器分散在两处；
// 传引用只会让装配代码变长，不会带来任何好处。
func GetRegistry() *Registry { return globalRegistry }

// Attach 接管一条新连接，阻塞直到它断开。
//
// 同一用户后来居上：一个人同时跑两个 agent 只会互相抢活——调用被其中一个
// 领走，另一个空转，用户完全看不出为什么"有时候没反应"。
func (r *Registry) Attach(user string, conn *Conn, workspace, version string) {
	s := newSession(user, workspace, version, conn)

	r.mu.Lock()
	old := r.sessions[user]
	r.sessions[user] = s
	r.mu.Unlock()

	if old != nil {
		log.Printf("[localagent] user=%s 转用新连接（旧连接被顶替）", user)
		old.Close()
	}

	log.Printf("[localagent] user=%s 已连接 workspace=%s version=%s", user, workspace, version)

	// 从注册表摘除 + 唤醒在途调用，无论是正常断开还是被顶替
	defer func() {
		r.mu.Lock()
		if cur, ok := r.sessions[user]; ok && cur == s {
			delete(r.sessions, user)
		}
		r.mu.Unlock()

		s.Close()
		log.Printf("[localagent] user=%s 已断开", user)
	}()

	go s.conn.PingLoop(s.done)
	s.readLoop()
}

// Current 返回该用户当前连接的本地 agent。
func (r *Registry) Current(user string) (*Session, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.sessions[user]
	return s, ok
}

// Online 返回当前在线的本地 agent 数量（可观测用）。
func (r *Registry) Online() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.sessions)
}
