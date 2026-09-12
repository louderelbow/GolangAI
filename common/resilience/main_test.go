package resilience

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestMain 用绝对路径指定示例配置，避免依赖测试进程的工作目录
// （config.GetConfig() 找不到文件会 log.Fatal 直接结束进程）
func TestMain(m *testing.M) {
	if _, file, _, ok := runtime.Caller(0); ok {
		root := filepath.Dir(filepath.Dir(filepath.Dir(file)))
		_ = os.Setenv("DEEPTALK_CONFIG", filepath.Join(root, "config", "config.toml.example"))
	}
	os.Exit(m.Run())
}
