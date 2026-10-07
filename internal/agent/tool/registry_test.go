package tool

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	einotool "github.com/cloudwego/eino/components/tool"
)

// TestRegistryRegisterAndInvoke 覆盖"工具注册可用"：
// 注册 → 取出 → 转成 eino 工具 → 真正调用一次。
func TestRegistryRegisterAndInvoke(t *testing.T) {
	r := NewRegistry()

	var called int32
	if err := r.Register(ToolSpec{
		Name:        "fake_echo",
		Description: "回声工具",
		Handler: func(ctx context.Context, args string) (string, error) {
			atomic.AddInt32(&called, 1)
			return "echo" + args, nil
		},
		Idempotent: true,
	}); err != nil {
		t.Fatalf("Register 失败: %v", err)
	}

	spec, ok := r.Get("fake_echo")
	if !ok {
		t.Fatal("注册后应能按名字取到工具")
	}

	inv, ok := spec.BaseTool().(einotool.InvokableTool)
	if !ok {
		t.Fatal("ToolSpec.BaseTool() 必须实现 eino 的 InvokableTool，否则 Agent 调不动它")
	}

	info, err := inv.Info(context.Background())
	if err != nil {
		t.Fatalf("Info 失败: %v", err)
	}
	if info.Name != "fake_echo" || info.Desc != "回声工具" {
		t.Fatalf("Info 内容不对: name=%q desc=%q", info.Name, info.Desc)
	}

	out, err := inv.InvokableRun(context.Background(), `{"q":"hi"}`)
	if err != nil {
		t.Fatalf("InvokableRun 失败: %v", err)
	}
	if out != `echo{"q":"hi"}` {
		t.Fatalf("返回值不对: %q", out)
	}
	if got := atomic.LoadInt32(&called); got != 1 {
		t.Fatalf("handler 应被调用 1 次，实际 %d 次", got)
	}
}

// TestRegistryRejectsBadSpec 空名字 / 无 handler 必须被拒绝，
// 否则会注册进一个永远调不通的工具。
func TestRegistryRejectsBadSpec(t *testing.T) {
	r := NewRegistry()
	noop := func(context.Context, string) (string, error) { return "", nil }

	if err := r.Register(ToolSpec{Name: "   ", Handler: noop}); err == nil {
		t.Fatal("空名字应被拒绝")
	}
	if err := r.Register(ToolSpec{Name: "no_handler"}); err == nil {
		t.Fatal("没有 handler 应被拒绝")
	}
	if names := r.Names(); len(names) != 0 {
		t.Fatalf("非法注册不应留下条目，实际 %v", names)
	}
}

// TestRegistryRejectsDuplicateName 重名必须报错并保留先注册的那个：
// 静默覆盖会让"工具没生效"变成极难排查的问题。
func TestRegistryRejectsDuplicateName(t *testing.T) {
	r := NewRegistry()
	h := func(context.Context, string) (string, error) { return "", nil }

	if err := r.Register(ToolSpec{Name: "dup", Handler: h}); err != nil {
		t.Fatalf("首次注册应成功: %v", err)
	}
	if err := r.Register(ToolSpec{Name: "dup", Handler: h}); err == nil {
		t.Fatal("重名应返回错误")
	}
	if got := len(r.Names()); got != 1 {
		t.Fatalf("重名不应新增条目，实际 %d 条", got)
	}
}

// TestToolSpecAppliesTimeout 单个工具卡住不能拖死整个 Agent 循环。
func TestToolSpecAppliesTimeout(t *testing.T) {
	var sawCancel atomic.Bool
	spec := ToolSpec{
		Name:    "slow_tool",
		Timeout: 50 * time.Millisecond,
		Handler: func(ctx context.Context, _ string) (string, error) {
			<-ctx.Done()
			sawCancel.Store(true)
			return "", ctx.Err()
		},
	}

	inv := spec.BaseTool().(einotool.InvokableTool)
	start := time.Now()
	if _, err := inv.InvokableRun(context.Background(), "{}"); err == nil {
		t.Fatal("超过 Timeout 应返回错误")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("超时未生效，实际耗时 %s", elapsed)
	}
	if !sawCancel.Load() {
		t.Fatal("handler 应观察到 ctx 被取消")
	}
}

// TestRegistryAggregatesSources 本地工具与外部来源（MCP）要能合并，
// 空来源不应贡献工具。
func TestRegistryAggregatesSources(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(ToolSpec{
		Name:    "local_a",
		Handler: func(context.Context, string) (string, error) { return "a", nil },
	}); err != nil {
		t.Fatalf("注册本地工具失败: %v", err)
	}

	r.AddSource(fakeSource{name: "mcp", tools: []einotool.BaseTool{
		ToolSpec{
			Name:    "remote_x",
			Handler: func(context.Context, string) (string, error) { return "x", nil },
		}.BaseTool(),
	}})
	r.AddSource(fakeSource{name: "empty"})

	got := r.Tools(context.Background())
	if len(got) != 2 {
		t.Fatalf("应合并出 2 个工具，实际 %d 个", len(got))
	}

	names := map[string]bool{}
	for _, bt := range got {
		info, err := bt.Info(context.Background())
		if err != nil {
			t.Fatalf("Info 失败: %v", err)
		}
		names[info.Name] = true
	}
	if !names["local_a"] || !names["remote_x"] {
		t.Fatalf("应同时包含本地与外部来源的工具，实际 %v", names)
	}
}

type fakeSource struct {
	name  string
	tools []einotool.BaseTool
}

func (s fakeSource) Name() string { return s.name }

func (s fakeSource) Tools(context.Context) []einotool.BaseTool { return s.tools }
