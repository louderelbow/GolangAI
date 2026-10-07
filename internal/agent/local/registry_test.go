package local

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// testUpgrader 只服务测试里的连接升级。
// 默认的 CheckOrigin 会拒绝所有请求，而测试客户端不发 Origin，正是我们想要的。
var testUpgrader = websocket.Upgrader{}

// 下面两个是**测试专用**的探针，定义在 _test.go 里而不是产品代码里：
// 生产路径不需要它们，把只读探针塞进 Session 只会让类型多两个没人调的方法。
func (s *Session) pendingCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.pending)
}

func (s *Session) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// startPair 起一条**真实**的 WebSocket 连接对：一边交给 Registry.Attach（扮演服务端），
// 另一边返回给测试（扮演用户电脑上的本地 agent）。
//
// 刻意不 mock 传输层：这套代码的价值几乎全在"两个进程隔着一条长连接对齐语义"
// 上——串行写、结果关联、断开唤醒、重连顶替——把它 mock 掉就等于不测。
func startPair(t *testing.T, reg *Registry, user string) *Conn {
	t.Helper()

	// 记下连接前的会话：重连场景下 Current() 会一直非 nil，
	// 只等"非 nil"会立刻返回旧的会话，测试就测不到顶替了。
	before, existedBefore := reg.Current(user)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := testUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		conn := NewConn(ws)
		// 真实流程里服务端要先读到 hello 才认这条连接
		if _, err := conn.Read(); err != nil {
			return
		}
		reg.Attach(user, conn, "/ws/root", "test")
	}))
	t.Cleanup(srv.Close)

	c, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	conn := NewConn(c)
	t.Cleanup(func() { _ = conn.Close() })

	if err := conn.Write(Frame{Type: FrameHello, Workspace: "/ws/root", Version: "test"}); err != nil {
		t.Fatalf("发送 hello: %v", err)
	}

	waitFor(t, func() bool {
		cur, ok := reg.Current(user)
		return ok && (!existedBefore || cur != before)
	}, "新的连接没有被登记")
	return conn
}

func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal(msg)
}

// TestInvokeRoundTrip 一次完整的请求-响应：服务端下发 call，本地回投 result。
func TestInvokeRoundTrip(t *testing.T) {
	reg := NewRegistry()
	cli := startPair(t, reg, "u1")

	go func() {
		f, err := cli.Read()
		if err != nil {
			return
		}
		if f.Type != FrameCall || f.Tool != "read_file" || f.Args != `{"path":"a.txt"}` {
			return
		}
		_ = cli.Write(Frame{Type: FrameResult, ID: f.ID, Output: "文件内容"})
	}()

	s, ok := reg.Current("u1")
	if !ok {
		t.Fatal("会话不存在")
	}
	out, err := s.Invoke(context.Background(), "read_file", `{"path":"a.txt"}`)
	if err != nil {
		t.Fatalf("Invoke 失败: %v", err)
	}
	if out != "文件内容" {
		t.Fatalf("输出不对: %q", out)
	}
}

// TestInvokeSurfacesRemoteError 对端回投的错误要能被调用方看到。
func TestInvokeSurfacesRemoteError(t *testing.T) {
	reg := NewRegistry()
	cli := startPair(t, reg, "u1")

	go func() {
		f, err := cli.Read()
		if err != nil {
			return
		}
		_ = cli.Write(Frame{Type: FrameResult, ID: f.ID, Error: "路径超出了工作区范围"})
	}()

	s, _ := reg.Current("u1")
	_, err := s.Invoke(context.Background(), "read_file", "{}")
	if err == nil || !strings.Contains(err.Error(), "工作区") {
		t.Fatalf("应把本地那端的拒绝原样带回来，实际 %v", err)
	}
}

// TestInvokeTimeout 对端不回结果时，调用方必须在自己超时后返回，
// 而不是无限等下去（工具层靠这个把"这一步没做成"回填给模型）。
func TestInvokeTimeout(t *testing.T) {
	reg := NewRegistry()
	_ = startPair(t, reg, "u1")

	s, _ := reg.Current("u1")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	if _, err := s.Invoke(ctx, "read_file", "{}"); err == nil {
		t.Fatal("对端不回结果时应当超时")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("超时应在 100ms 左右返回，实际 %s", elapsed)
	}
}

// TestDisconnectFailsPendingInvoke 用户关掉 agent 时，在途调用要立刻被唤醒。
//
// 这是本文件里最值钱的一条：如果断开不能唤醒，每个在途工具都要白等满
// 十几秒超时——用户看到的是"卡住了"，而其实早就没救了。
func TestDisconnectFailsPendingInvoke(t *testing.T) {
	reg := NewRegistry()
	cli := startPair(t, reg, "u1")

	s, _ := reg.Current("u1")
	errCh := make(chan error, 1)
	go func() {
		_, err := s.Invoke(context.Background(), "read_file", "{}")
		errCh <- err
	}()

	// 等调用真的被下发出去，再模拟用户合盖 / 断网
	waitFor(t, func() bool { return s.pendingCount() == 1 }, "调用没有被登记为在途")
	_ = cli.Close()

	select {
	case err := <-errCh:
		if !errors.Is(err, ErrAgentGone) {
			t.Fatalf("应是 ErrAgentGone（结果未知），实际 %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("断开后应立即唤醒在途调用，而不是干等超时")
	}
}

// TestReconnectTakesOver 同一用户重连时，旧连接被顶替。
//
// 不顶替的话，两个 agent 会互相抢活：调用被其中一个领走，另一个空转，
// 用户完全看不出为什么"有时候没反应"。
func TestReconnectTakesOver(t *testing.T) {
	reg := NewRegistry()
	_ = startPair(t, reg, "u1")
	firstSession, _ := reg.Current("u1")

	second := startPair(t, reg, "u1")
	secondSession, _ := reg.Current("u1")

	if firstSession == secondSession {
		t.Fatal("重连后应是新的会话对象")
	}
	waitFor(t, func() bool { return firstSession.isClosed() }, "旧会话应被关闭")
	_ = second.Close()
}

// TestSetWorkspaceRoundTrip 更换工作区走的是与工具调用同一套请求-响应。
//
// 之所以能共用：两者都是"服务端请求、本地执行、回投结果"，差别只在帧类型。
// 合成一条路径是为了让超时、断开唤醒、迟到丢弃这些语义只有一份实现。
func TestSetWorkspaceRoundTrip(t *testing.T) {
	reg := NewRegistry()
	cli := startPair(t, reg, "u1")

	s, _ := reg.Current("u1")
	if ws, _, _ := s.Info(); ws != "/ws/root" {
		t.Fatalf("初始工作区不对: %q", ws)
	}

	go func() {
		f, err := cli.Read()
		if err != nil {
			return
		}
		if f.Type != FrameSetWorkspace || f.Args != "/new/dir" {
			return
		}
		_ = cli.Write(Frame{Type: FrameWorkspace, ID: f.ID, OK: true, Workspace: "/new/dir"})
	}()

	got, err := s.SetWorkspace(context.Background(), "/new/dir")
	if err != nil {
		t.Fatalf("SetWorkspace 失败: %v", err)
	}
	if got != "/new/dir" {
		t.Fatalf("返回的工作区不对: %q", got)
	}
	if ws, _, _ := s.Info(); ws != "/new/dir" {
		t.Fatalf("会话没有记住新工作区: %q", ws)
	}
}

// TestSetWorkspaceFailurePropagates 本机拒绝（例如用户在目录框里点了取消）
// 要能原样传到网页上，而且**不能**改动当前工作区。
func TestSetWorkspaceFailurePropagates(t *testing.T) {
	reg := NewRegistry()
	cli := startPair(t, reg, "u1")
	s, _ := reg.Current("u1")

	go func() {
		f, err := cli.Read()
		if err != nil {
			return
		}
		_ = cli.Write(Frame{Type: FrameWorkspace, ID: f.ID, OK: false, Error: "没有选择目录"})
	}()

	_, err := s.SetWorkspace(context.Background(), "")
	if err == nil || !strings.Contains(err.Error(), "没有选择目录") {
		t.Fatalf("本机的拒绝理由应原样带上来，实际 %v", err)
	}
	if ws, _, _ := s.Info(); ws != "/ws/root" {
		t.Fatalf("失败不该改动工作区，实际 %q", ws)
	}
}

// TestSetWorkspaceTimeout 用户迟迟不在目录框里做选择时，服务端不能无限等。
func TestSetWorkspaceTimeout(t *testing.T) {
	reg := NewRegistry()
	_ = startPair(t, reg, "u1")
	s, _ := reg.Current("u1")

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	if _, err := s.SetWorkspace(ctx, ""); err == nil {
		t.Fatal("对端不回应时应当超时返回")
	}
}

// TestUnregisterDoesNotKickNewConnection 旧连接断开时不能把新连接一起摘掉。
func TestUnregisterDoesNotKickNewConnection(t *testing.T) {
	reg := NewRegistry()
	first := startPair(t, reg, "u1")

	_ = startPair(t, reg, "u1") // 顶替

	// 让旧连接彻底断开，触发它自己的清理
	_ = first.Close()
	time.Sleep(150 * time.Millisecond)

	if _, ok := reg.Current("u1"); !ok {
		t.Fatal("旧连接的清理不该把已经顶替它的新连接一起摘掉")
	}
}
