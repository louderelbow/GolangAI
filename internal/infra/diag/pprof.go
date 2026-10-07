// Package diag 提供只在诊断/压测时开启的调试端点。
//
// 为什么不直接挂在 Gin 的端口上：
//  1. /debug/pprof 能 dump 出完整的调用栈和堆内存，等于把内部结构暴露给任何人，
//     绝对不能和业务接口共用一个对外端口；
//  2. 默认只监听 127.0.0.1，要远程看就自己开 SSH 隧道（ssh -L 6060:127.0.0.1:6060 ...）。
//
// 为什么在这里开 block/mutex 采样：
//
//	Go 的 goroutine/heap profile 默认就可用，但**阻塞**和**锁竞争**两个 profile 默认是关的。
//	压测要定位的恰恰是这两类问题（比如全局 metrics 注册表的互斥锁争用），所以打开开关时一并采样。
package diag

import (
	"context"
	"log"
	"net/http"
	"net/http/pprof"
	"runtime"
	"time"
)

// StartPprof 在 addr 上启动一个只提供 /debug/pprof 的 HTTP 服务。
// 返回的 Server 需要在退出时调用 Shutdown。
func StartPprof(addr string) *http.Server {
	// 开启阻塞与锁竞争采样（默认关闭）。采样有性能开销，所以只在显式打开诊断时启用。
	runtime.SetBlockProfileRate(1)     // 每次阻塞事件都记录（1 = 全采样）
	runtime.SetMutexProfileFraction(1) // 每次锁竞争都记录

	// 用独立 mux，只暴露 pprof，不把 DefaultServeMux 上的任何其它东西带出去
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		log.Printf("[diag] pprof listening on http://%s/debug/pprof/ (block/mutex sampling ON)", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("[diag] pprof server error: %v", err)
		}
	}()

	return srv
}

// ShutdownPprof 优雅关闭 pprof 服务
func ShutdownPprof(srv *http.Server) {
	if srv == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("[diag] pprof shutdown failed: %v", err)
	}
}
