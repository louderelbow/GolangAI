// Command localagent 跑在**用户自己的电脑**上，替服务端的 Agent 干活。
//
// 它只做三件事：连上服务端、领活、干活回投。模型与决策全在服务端，
// 本地这端不持有任何模型密钥——这也是"让 AI 操作本地文件"最安全的切法：
// 能碰硬盘的进程不联网做决策，做决策的进程碰不到硬盘。
//
// 用法：
//
//	# 工作区就是当前目录
//	deeptalk-agent -server http://127.0.0.1:9090 -token <你的 JWT>
//
//	# 指定工作区
//	deeptalk-agent -server http://127.0.0.1:9090 -token <JWT> -workspace D:\code\myproj
//
// token 也可以放在环境变量 DEEPTALK_TOKEN 里，免得进 shell 历史。
//
// 关于安全：服务端能做的只是"请求"。真正决定某个路径能不能碰的是本文件的
// resolve——它挡住 ".." 穿越和符号链接逃逸。将来要加写文件/执行命令，
// 确认动作也应该加在这里（用户在自己电脑上点"允许"），而不是在服务端：
// 服务端如果被攻破，它不该具备"绕过本机确认"的能力。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	agentlocal "deeptalk/internal/agent/local"

	"github.com/gorilla/websocket"
)

// version 会在 hello 帧里上报，方便服务端日志区分新旧客户端。
const version = "0.1.0"

// 读取上限：一个文件读进来是要进模型上下文的，不能无限大。
// 超出部分截断并在结果里注明——比默默截断好，模型能据此决定分段读。
const (
	maxReadBytes   = 256 << 10 // 单次读取上限 256KB
	maxListEntries = 300       // 单次列目录上限
)

var errOutsideWorkspace = errors.New("路径超出了工作区范围，已拒绝")

func main() {
	server := flag.String("server", "", "DeepTalk 服务端地址；留空则用上次记住的（默认 127.0.0.1:9090）")
	workspace := flag.String("workspace", "", "工作区目录；留空且从未选过时会弹出系统目录选择框")
	token := flag.String("token", "", "登录 token；留空则读环境变量 DEEPTALK_TOKEN 或上次记住的值")
	flag.Parse()

	cfg := loadConfig()

	// 优先级：命令行 > 环境变量 > 上次记住的。
	// 设置一次之后就不用再传任何参数，直接双击运行。
	if v := strings.TrimRight(strings.TrimSpace(*server), "/"); v != "" {
		cfg.Server = v
	}
	if v := strings.TrimSpace(*token); v != "" {
		cfg.Token = v
	} else if v := strings.TrimSpace(os.Getenv("DEEPTALK_TOKEN")); v != "" {
		cfg.Token = v
	}
	if v := strings.TrimSpace(*workspace); v != "" {
		cfg.Workspace, cfg.WorkspaceChosen = v, true
	}
	if cfg.Server == "" {
		cfg.Server = "http://127.0.0.1:9090"
	}
	if cfg.Token == "" {
		log.Fatalf("还没有连接过。首次运行请传一次 -token（或设置环境变量 DEEPTALK_TOKEN），之后会被记住。")
	}

	// 只有首次运行（或上次的目录没了）才弹选择框。
	// 每次启动都弹的话，比回终端敲命令还烦。
	if !cfg.WorkspaceChosen || cfg.Workspace == "" {
		root, err := chooseWorkspace(cfg.Workspace)
		if err != nil {
			log.Fatalf("需要先选一个工作区: %v", err)
		}
		cfg.Workspace, cfg.WorkspaceChosen = root, true
	} else if _, err := resolveWorkspace(cfg.Workspace); err != nil {
		log.Printf("上次的工作区已不可用（%v），重新选择", err)
		root, pickErr := chooseWorkspace(cfg.Workspace)
		if pickErr != nil {
			log.Fatalf("需要先选一个工作区: %v", pickErr)
		}
		cfg.Workspace = root
	}

	saveConfig(cfg)

	a := &agent{server: cfg.Server, token: cfg.Token, root: cfg.Workspace, cfg: cfg}

	log.Printf("工作区: %s", a.root)
	log.Printf("服务端: %s", a.server)
	log.Printf("配置已记住（%s）—— 之后直接运行本程序即可，不用再传参数", configPath())
	log.Printf("本进程只读工作区内的文件；写与执行尚未开放")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	a.run(ctx)
}

// resolveWorkspace 把 -workspace 解析成绝对路径并**解析掉符号链接**。
//
// 必须解析软链：后面所有"是否在工作区内"的判断都拿这个 root 做基准，
// 如果 root 本身是个软链而校验时用的又是它逻辑上的路径，就会出现
// "校验通过但实际落在别处"的情况。
func resolveWorkspace(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	st, err := os.Stat(real)
	if err != nil {
		return "", err
	}
	if !st.IsDir() {
		return "", fmt.Errorf("%s 不是目录", real)
	}
	return real, nil
}

// wsURL 把 http(s):// 形式的服务端地址转成 ws(s)://。
//
// gorilla 的 Dialer 只认 ws / wss，直接塞 http:// 会以
// "malformed ws or wss URL" 失败——这个报错完全指不到病根，
// 所以在入口处转一次，而不是让用户对着日志猜。
func wsURL(server string) string {
	switch {
	case strings.HasPrefix(server, "https://"):
		return "wss://" + strings.TrimPrefix(server, "https://")
	case strings.HasPrefix(server, "http://"):
		return "ws://" + strings.TrimPrefix(server, "http://")
	default:
		return server
	}
}

type agent struct {
	server string
	token  string
	root   string
	cfg    agentConfig
}

// run 不断重连，直到 ctx 被取消。
//
// 断线重连是必须的：用户可能先开 agent 再启服务端、笔记本可能休眠、
// 服务端可能重启。让用户每次都手动重启 agent 是不能接受的。
func (a *agent) run(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		start := time.Now()
		err := a.session(ctx)
		if ctx.Err() != nil {
			break
		}
		if time.Since(start) > 30*time.Second {
			backoff = time.Second // 连上过并稳定运行，重置退避
		}
		log.Printf("连接中断: %v；%s 后重连", err, backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 15*time.Second {
			backoff *= 2
		}
	}
	log.Printf("已退出")
}

// session 建立一次连接并服务到断开。
func (a *agent) session(ctx context.Context) error {
	header := http.Header{}
	header.Set("Authorization", "Bearer "+a.token)

	url := wsURL(a.server) + "/api/v1/agent/ws"
	ws, resp, err := websocket.DefaultDialer.DialContext(ctx, url, header)
	if err != nil {
		if resp != nil {
			return fmt.Errorf("连接 %s 失败: HTTP %d", url, resp.StatusCode)
		}
		return fmt.Errorf("连接 %s 失败: %w", url, err)
	}
	conn := agentlocal.NewConn(ws)
	defer conn.Close()

	if err := conn.Write(agentlocal.Frame{
		Type:      agentlocal.FrameHello,
		Workspace: a.root,
		Version:   version,
	}); err != nil {
		return fmt.Errorf("发送 hello 失败: %w", err)
	}
	log.Printf("已连接")

	// 心跳：ctx 结束时停止
	stop := make(chan struct{})
	defer close(stop)
	go conn.PingLoop(stop)

	for {
		f, err := conn.Read()
		if err != nil {
			return err
		}

		switch f.Type {
		case agentlocal.FrameCall:
			// 把每次要执行的动作打出来。这是"用户看得见 agent 在干什么"
			// 的最低成本形式；将来加写/执行时，确认对话也该长在这条路径上。
			log.Printf("→ %s %s", f.Tool, truncate(f.Args, 200))

			out, callErr := a.execute(f)
			res := agentlocal.Frame{Type: agentlocal.FrameResult, ID: f.ID, Output: out}
			if callErr != nil {
				res.Error = callErr.Error()
				log.Printf("← %s 失败: %v", f.Tool, callErr)
			} else {
				// 只打结果的首行并截断：拒绝理由要看得见（"越界被拒"是用户
				// 应该知道的事），但绝不把整份文件内容倒进终端。
				log.Printf("← %s %s", f.Tool, truncate(firstLine(out), 120))
			}
			if err := conn.Write(res); err != nil {
				return fmt.Errorf("回投结果失败: %w", err)
			}

		case agentlocal.FrameSetWorkspace:
			a.handleSetWorkspace(conn, f)

		default:
			log.Printf("忽略未知帧 type=%q", f.Type)
		}
	}
}

// handleSetWorkspace 处理网页发起的"更换工作区"。
//
// 无论服务端有没有带路径，最终都由**用户在自己电脑上**通过选择框确认；
// 服务端给的路径只作为选择框的初始位置。
//
// 这一点是刻意的：工作区就是本地 agent 能触碰的边界。如果服务端能直接改它，
// 一个被攻破（或只是配错）的服务端就可以先把工作区设成 C:\ 再读光整块盘，
// 那么下面那两层路径校验就完全失去意义。弹框把这个决定权留在用户手里。
func (a *agent) handleSetWorkspace(conn *agentlocal.Conn, f agentlocal.Frame) {
	initial := strings.TrimSpace(f.Args)
	if initial == "" {
		initial = a.root
	}
	log.Printf("→ 收到更换工作区请求，本机将弹出目录选择框")

	root, err := chooseWorkspace(initial)
	if err != nil {
		log.Printf("← 更换工作区失败: %v", err)
		_ = conn.Write(agentlocal.Frame{
			Type: agentlocal.FrameWorkspace, ID: f.ID, OK: false, Error: err.Error(),
		})
		return
	}

	a.root = root
	a.cfg.Workspace, a.cfg.WorkspaceChosen = root, true
	saveConfig(a.cfg)

	log.Printf("← 工作区已切换为 %s", root)
	_ = conn.Write(agentlocal.Frame{
		Type: agentlocal.FrameWorkspace, ID: f.ID, OK: true, Workspace: root,
	})
}

// execute 分发一次工具调用。
//
// 返回值的约定很重要：**应用层的正常结局走 output，只有真正的故障才走 error。**
//
// 区别在哪：返回 error 会被上层当成"工具挂了"——给用户弹一条"没取到数据"
// 的告警，还会累加 read_file 的熔断计数。而"路径越界""文件不存在"根本不是
// 故障，它是这次调用得到的**答案**。更要命的是上层的故障文案会写着
// "可以换一种方式重试"，于是一个猜错路径的模型会被鼓励反复重试，
// 试满五次就把熔断器打开，连合法的读取也一起打死。
//
// 所以只有"这个工具压根不该被这样调用"（未知工具名）才算 error。
func (a *agent) execute(f agentlocal.Frame) (string, error) {
	switch f.Tool {
	case "list_files":
		return a.listFiles(f.Args)
	case "read_file":
		return a.readFile(f.Args)
	default:
		return "", fmt.Errorf("未知工具 %q：本地 agent 版本可能比服务端旧", f.Tool)
	}
}

// soft 把可预期的失败变成给模型看的一句话（error 为 nil）。
// 用法见 execute 上方的说明：这些是"答案"，不是"故障"。
func soft(format string, args ...any) (string, error) {
	return fmt.Sprintf(format, args...), nil
}

type pathArgs struct {
	Path string `json:"path"`
}

// resolve 把模型给的路径解析成工作区内的绝对路径。
//
// 这是本地这端唯一真正的安全边界。模型可以把 path 写成
// "../../../etc/passwd"，也可以借一个指向外部的符号链接跳出去，
// 所以校验必须分两层：
//
//  1. 解析后的路径要落在工作区内 —— 挡住 ".." 穿越
//  2. 解析符号链接后的**真实路径**也要落在工作区内 —— 挡住软链
//
// 只做第 1 层是不够的：`ln -s / escape` 之后 `escape/etc/passwd`
// 逻辑上完全"在工作区内"。
//
// 绝对路径是允许的，但同样受上面两层约束。早先的版本一律拒绝绝对路径，
// 理由是"简单"——结果用户最自然的说法「读一下 C:\code\proj\README.md」
// 必然失败，模型只能猜一个相对路径再试一次。而"必须落在工作区内"这条
// 约束本来就由这两层校验兜住了，放开绝对路径并不削弱安全性。
func (a *agent) resolve(rel string) (string, error) {
	rel = strings.TrimSpace(rel)
	if rel == "" {
		rel = "."
	}

	var joined string
	if filepath.IsAbs(rel) {
		joined = filepath.Clean(rel)
	} else {
		joined = filepath.Join(a.root, rel)
	}

	if !within(a.root, joined) {
		return "", errOutsideWorkspace
	}

	// EvalSymlinks 要求路径存在。不存在时（例如将来要写一个新文件）
	// 退回校验它的父目录——绝不能因为"它还不存在"就跳过安全校验。
	real, err := filepath.EvalSymlinks(joined)
	if err != nil {
		parent, perr := filepath.EvalSymlinks(filepath.Dir(joined))
		if perr != nil {
			return "", fmt.Errorf("无法解析路径: %w", err)
		}
		real = filepath.Join(parent, filepath.Base(joined))
	}
	if !within(a.root, real) {
		return "", errOutsideWorkspace
	}
	return real, nil
}

// within 判断 p 是否在 root 之内（root 自身算在内）。
func within(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return false
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return !filepath.IsAbs(rel)
}

// display 把绝对路径转回相对工作区的展示形式，避免把用户的家目录路径
// 一次次写进对话历史。
func (a *agent) display(abs string) string {
	if rel, err := filepath.Rel(a.root, abs); err == nil {
		return filepath.ToSlash(rel)
	}
	return filepath.ToSlash(abs)
}

// listFiles 列出工作区内的一个目录。
func (a *agent) listFiles(argsJSON string) (string, error) {
	var args pathArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return soft(`参数格式不对，应为 {"path":"目录相对路径"}`)
	}

	full, err := a.resolve(args.Path)
	if err != nil {
		return soft("%v。工作区根是 %q，路径需在它内部（相对路径，或工作区内的绝对路径）；越界路径不会成功，请不要再试。",
			err, a.root)
	}

	entries, err := os.ReadDir(full)
	if err != nil {
		return soft("无法列出 %s：%v", a.display(full), err)
	}
	sort.Slice(entries, func(i, j int) bool {
		// 目录在前，同类按名字排——和用户在文件管理器里看到的一致
		di, dj := entries[i].IsDir(), entries[j].IsDir()
		if di != dj {
			return di
		}
		return entries[i].Name() < entries[j].Name()
	})

	var b strings.Builder
	fmt.Fprintf(&b, "工作区根：%s\n", a.root)
	fmt.Fprintf(&b, "当前目录：%s\n", a.display(full))
	fmt.Fprintf(&b, "条目：%d\n\n", len(entries))

	shown := 0
	for _, e := range entries {
		if shown >= maxListEntries {
			fmt.Fprintf(&b, "\n…还有 %d 个条目未列出（上限 %d）\n", len(entries)-shown, maxListEntries)
			break
		}
		shown++

		info, statErr := e.Info()
		kind, size, mod := "?", "-", "-"
		switch {
		case e.IsDir():
			kind, size = "d", "-"
		case e.Type()&os.ModeSymlink != 0:
			kind, size = "l", "-"
		}
		if statErr == nil {
			if !e.IsDir() {
				size = humanSize(info.Size())
			}
			mod = info.ModTime().Format("2006-01-02 15:04")
		}

		name := e.Name()
		if e.IsDir() {
			name += "/"
		}
		fmt.Fprintf(&b, "%s  %8s  %s  %s\n", kind, size, mod, name)
	}
	return b.String(), nil
}

// readFile 读取工作区内的一个文件。
func (a *agent) readFile(argsJSON string) (string, error) {
	var args pathArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return soft(`参数格式不对，应为 {"path":"文件相对路径"}`)
	}

	full, err := a.resolve(args.Path)
	if err != nil {
		return soft("%v。工作区根是 %q，路径需在它内部（相对路径，或工作区内的绝对路径）；越界路径不会成功，请不要再试。",
			err, a.root)
	}

	st, err := os.Stat(full)
	if err != nil {
		return soft("无法访问 %s：%v", a.display(full), err)
	}
	if st.IsDir() {
		return soft("%s 是目录，请用 list_files 查看，或指定具体文件", a.display(full))
	}

	f, err := os.Open(full)
	if err != nil {
		return soft("无法打开 %s：%v", a.display(full), err)
	}
	defer f.Close()

	// 多读 1 字节用来判断"是否被截断"
	data, err := io.ReadAll(io.LimitReader(f, maxReadBytes+1))
	if err != nil {
		return soft("读取 %s 时出错：%v", a.display(full), err)
	}

	truncated := false
	if len(data) > maxReadBytes {
		data = data[:maxReadBytes]
		truncated = true
	}

	if looksBinary(data) {
		return fmt.Sprintf("%s 看起来是二进制文件（%s），没有读取内容。若确实需要，请换用能解释它的工具。",
			a.display(full), humanSize(st.Size())), nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "文件：%s\n大小：%s\n\n", a.display(full), humanSize(st.Size()))
	b.Write(data)
	if truncated {
		fmt.Fprintf(&b, "\n\n…内容已被截断（只读了前 %s，完整大小 %s）",
			humanSize(maxReadBytes), humanSize(st.Size()))
	}
	return b.String(), nil
}

// looksBinary 用"前 8KB 里有没有 NUL"做判断。
//
// 这是个粗糙但足够可靠的启发式：文本文件里出现 \x00 的概率极低，
// 而它挡住的收益很大——把一段二进制塞进上下文既浪费 token
// 又会让模型产生幻觉。
func looksBinary(data []byte) bool {
	n := len(data)
	if n > 8192 {
		n = 8192
	}
	for i := 0; i < n; i++ {
		if data[i] == 0 {
			return true
		}
	}
	return false
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	units := []string{"KB", "MB", "GB"}
	v := float64(n)
	for _, u := range units {
		v /= unit
		if v < unit {
			return fmt.Sprintf("%.1f%s", v, u)
		}
	}
	return fmt.Sprintf("%.1fTB", v/unit)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// firstLine 取首行：日志里只需要看到"这次调用得到的结论"，
// 文件正文属于结果本身，不该被复制进终端。
func firstLine(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		return s[:i]
	}
	return s
}
