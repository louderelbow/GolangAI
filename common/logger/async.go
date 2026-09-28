package logger

import (
	"io"
	stdlog "log"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// ======================== 异步批量日志输出 ========================
//
// 起因（压测 + CPU profile 发现的）：
// 日志原本是同步直写 stdout。在 Windows 上 stdout 是控制台，走的是 WriteConsole
// 系统调用——20 秒 26 RPS 的 profile 里这一项独占 13.5% 的 CPU：
//
//	syscall.WriteConsole   13.53%
//	os.(*File).Write       13.79%
//	log.(*Logger).output   14.32%
//
// 更麻烦的是它是**同步**的：每个打日志的 goroutine 都要等 I/O 返回，请求延迟被日志拖住。
//
// 修法：入队即返回，后台 goroutine 攒够一批（或到时间）一次写出。
//   - 一次 Write 变成一次批量 Write，系统调用次数降一两个数量级；
//   - 业务 goroutine 不再等 I/O；
//   - 队列满时**丢弃**而不是阻塞——日志不能反过来把业务拖死，丢了多少条会计数。

const (
	defaultQueueSize  = 4096             // 队列长度（条）
	defaultBufSize    = 64 << 10         // 攒到 64KB 就写一次
	defaultFlushEvery = 200 * time.Millisecond
)

// AsyncWriter 异步批量写 io.Writer，可直接喂给 log.SetOutput / gin.DefaultWriter
type AsyncWriter struct {
	out        io.Writer
	ch         chan []byte
	quit       chan struct{}
	done       chan struct{}
	bufSize    int
	flushEvery time.Duration

	closed  atomic.Bool
	dropped atomic.Int64
	started sync.Once
}

// NewAsyncWriter 创建异步写入器并启动后台协程
func NewAsyncWriter(out io.Writer, queueSize, bufSize int, flushEvery time.Duration) *AsyncWriter {
	if queueSize <= 0 {
		queueSize = defaultQueueSize
	}
	if bufSize <= 0 {
		bufSize = defaultBufSize
	}
	if flushEvery <= 0 {
		flushEvery = defaultFlushEvery
	}
	w := &AsyncWriter{
		out:        out,
		ch:         make(chan []byte, queueSize),
		quit:       make(chan struct{}),
		done:       make(chan struct{}),
		bufSize:    bufSize,
		flushEvery: flushEvery,
	}
	w.started.Do(func() { go w.run() })
	return w
}

// Write 实现 io.Writer：拷贝入队后立刻返回，不阻塞调用方
func (w *AsyncWriter) Write(p []byte) (int, error) {
	if w == nil {
		return len(p), nil
	}
	// 关闭后（正常只在进程退出阶段）退化成同步写，避免日志丢失
	if w.closed.Load() {
		return w.out.Write(p)
	}

	// 必须拷贝：调用方（标准库 log）会复用底层数组
	b := make([]byte, len(p))
	copy(b, p)

	select {
	case w.ch <- b:
	default:
		// 队列满：丢弃并计数。宁可丢日志，也不能让日志把请求堵死。
		w.dropped.Add(1)
	}
	return len(p), nil
}

func (w *AsyncWriter) run() {
	defer close(w.done)

	ticker := time.NewTicker(w.flushEvery)
	defer ticker.Stop()

	buf := make([]byte, 0, w.bufSize)
	flush := func() {
		if len(buf) == 0 {
			return
		}
		_, _ = w.out.Write(buf)
		buf = buf[:0]
	}

	for {
		select {
		case b := <-w.ch:
			buf = append(buf, b...)
			if len(buf) >= w.bufSize {
				flush()
			}
		case <-ticker.C:
			flush()
		case <-w.quit:
			// 退出前把队列里剩下的都写完，再 flush
			for {
				select {
				case b := <-w.ch:
					buf = append(buf, b...)
					continue
				default:
				}
				break
			}
			flush()
			return
		}
	}
}

// Close 停止异步写入并把剩余日志刷出去（超时后放弃，不阻塞退出）
func (w *AsyncWriter) Close(timeout time.Duration) {
	if w == nil {
		return
	}
	if !w.closed.CompareAndSwap(false, true) {
		return // 已关闭
	}
	close(w.quit)
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	select {
	case <-w.done:
	case <-time.After(timeout):
	}
}

// Dropped 队列满被丢弃的日志条数（0 表示没丢过）
func (w *AsyncWriter) Dropped() int64 {
	if w == nil {
		return 0
	}
	return w.dropped.Load()
}

// ---------------- 全局开关 ----------------

var (
	asyncMu  sync.RWMutex
	asyncOut *AsyncWriter
)

// EnableAsyncOutput 把标准库 log 的输出切到异步批量写，并返回写入器。
// 调用后可通过 Output() 拿到它（供 gin.DefaultWriter 使用）。
func EnableAsyncOutput() *AsyncWriter {
	asyncMu.Lock()
	defer asyncMu.Unlock()
	if asyncOut != nil {
		return asyncOut
	}
	asyncOut = NewAsyncWriter(os.Stdout, defaultQueueSize, defaultBufSize, defaultFlushEvery)
	// 标准库 log 也切过去：项目里 log.Printf 用得比 slog 多得多
	stdlog.SetOutput(asyncOut)
	return asyncOut
}

// Output 返回当前日志输出目标：开启异步后返回异步写入器，否则是 os.Stdout
func Output() io.Writer {
	asyncMu.RLock()
	defer asyncMu.RUnlock()
	if asyncOut != nil {
		return asyncOut
	}
	return os.Stdout
}

// CloseOutput 停止异步写入并 flush（进程退出前调用）
func CloseOutput(timeout time.Duration) {
	asyncMu.RLock()
	w := asyncOut
	asyncMu.RUnlock()
	w.Close(timeout)
}

// DroppedLogs 被丢弃的日志条数
func DroppedLogs() int64 {
	asyncMu.RLock()
	w := asyncOut
	asyncMu.RUnlock()
	return w.Dropped()
}
