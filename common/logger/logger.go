package logger

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

var log *slog.Logger

// contextKey 用于从 context 中取 requestId
type contextKey string

const RequestIDKey contextKey = "requestId"

// Init 初始化日志器，level: debug/info/warn/error
func Init(level string) {
	var l slog.Level
	switch strings.ToLower(level) {
	case "debug":
		l = slog.LevelDebug
	case "warn":
		l = slog.LevelWarn
	case "error":
		l = slog.LevelError
	default:
		l = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{
		Level: l,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				a.Value = slog.StringValue(time.Now().Format("2006-01-02 15:04:05.000"))
			}
			return a
		},
	}
	handler := slog.NewTextHandler(os.Stdout, opts)
	log = slog.New(handler)
	slog.SetDefault(log)
}

// logWithCaller 带调用位置和 requestId 的日志
func logWithCaller(ctx context.Context, level slog.Level, module, msg string, args ...any) {
	if log == nil {
		Init("info")
	}

	var attrs []slog.Attr
	attrs = append(attrs, slog.String("mod", module))

	// 从 context 取 requestId
	if ctx != nil {
		if rid, ok := ctx.Value(RequestIDKey).(string); ok && rid != "" {
			attrs = append(attrs, slog.String("rid", rid))
		}
	}

	// 调用者位置
	_, file, line, ok := runtime.Caller(2)
	if ok {
		file = filepath.Base(file)
		attrs = append(attrs, slog.String("at", fmt.Sprintf("%s:%d", file, line)))
	}

	// 拼接额外参数
	for i := 0; i < len(args); i += 2 {
		key := fmt.Sprintf("%v", args[i])
		val := ""
		if i+1 < len(args) {
			val = fmt.Sprintf("%v", args[i+1])
		}
		attrs = append(attrs, slog.String(key, val))
	}

	log.LogAttrs(ctx, level, msg, attrs...)
}

func Info(ctx context.Context, module, msg string, args ...any) {
	logWithCaller(ctx, slog.LevelInfo, module, msg, args...)
}

func Warn(ctx context.Context, module, msg string, args ...any) {
	logWithCaller(ctx, slog.LevelWarn, module, msg, args...)
}

func Error(ctx context.Context, module, msg string, args ...any) {
	logWithCaller(ctx, slog.LevelError, module, msg, args...)
}

func Debug(ctx context.Context, module, msg string, args ...any) {
	logWithCaller(ctx, slog.LevelDebug, module, msg, args...)
}
