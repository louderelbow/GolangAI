package tool

import (
	"context"
	"strings"
	"testing"
)

func TestLoopGuardBlocksAfterThreshold(t *testing.T) {
	g := NewLoopGuard(3)

	// 前 3 次放行（Blocked 在调用**前**问，所以第 1 次问时计数还是 0）
	for i := 1; i <= 3; i++ {
		if n, _, blocked := g.Blocked("list_files", `{"path":"."}`); blocked {
			t.Fatalf("第 %d 次不该被拦（当时成功次数 %d）", i, n)
		}
		g.Succeeded("list_files", `{"path":"."}`)
	}

	// 第 4 次拦下来，并报出已有的成功次数
	n, _, blocked := g.Blocked("list_files", `{"path":"."}`)
	if !blocked {
		t.Fatal("第 4 次应当被拦")
	}
	if n != 3 {
		t.Fatalf("应报出 3 次成功，实际 %d", n)
	}
}

// TestLoopGuardTreatsArgOrderAsSame 参数键顺序不同必须视为同一次调用。
//
// 模型每次生成的 JSON 键顺序可能不一样（{"a":1,"b":2} 与 {"b":2,"a":1}），
// 如果按字节比较，同一个死循环会被当成两个不同的调用，检测直接失效。
func TestLoopGuardTreatsArgOrderAsSame(t *testing.T) {
	g := NewLoopGuard(2)

	g.Succeeded("search", `{"q":"golang","limit":10}`)
	g.Succeeded("search", `{"limit":10,"q":"golang"}`)

	if _, _, blocked := g.Blocked("search", `{"q":"golang","limit":10}`); !blocked {
		t.Fatal("键顺序不同但语义相同，应当已被判定为循环")
	}
}

// TestLoopGuardSeparatesTools 不同工具各自计数。
func TestLoopGuardSeparatesTools(t *testing.T) {
	g := NewLoopGuard(2)

	g.Succeeded("read_file", `{"path":"a"}`)
	g.Succeeded("list_files", `{"path":"a"}`)

	if _, _, blocked := g.Blocked("read_file", `{"path":"a"}`); blocked {
		t.Fatal("read_file 只成功过 1 次，不该被拦")
	}
	if _, _, blocked := g.Blocked("list_files", `{"path":"a"}`); blocked {
		t.Fatal("list_files 只成功过 1 次，不该被拦")
	}
}

// TestLoopGuardSeparatesArgs 同工具不同参数不算循环。
func TestLoopGuardSeparatesArgs(t *testing.T) {
	g := NewLoopGuard(2)

	g.Succeeded("read_file", `{"path":"a.txt"}`)
	g.Succeeded("read_file", `{"path":"b.txt"}`)
	g.Succeeded("read_file", `{"path":"c.txt"}`)

	if _, _, blocked := g.Blocked("read_file", `{"path":"d.txt"}`); blocked {
		t.Fatal("读不同文件是正常行为，不该被拦")
	}
}

// TestLoopGuardNilSafe 没有守卫（单测、或功能关闭）时必须安静退化。
func TestLoopGuardNilSafe(t *testing.T) {
	var g *LoopGuard
	if _, _, blocked := g.Blocked("x", "{}"); blocked {
		t.Fatal("nil guard 不该拦任何东西")
	}
	g.Succeeded("x", "{}") // 不应 panic

	// ctx 里没放守卫时同理
	if got := loopGuardFrom(context.Background()); got != nil {
		t.Fatal("空 ctx 应当取不到守卫")
	}
}

// TestLoopGuardNonJSONArgs 参数不是合法 JSON 时也要能检测（退化成按原文比较）。
func TestLoopGuardNonJSONArgs(t *testing.T) {
	g := NewLoopGuard(2)
	g.Succeeded("weird", "not-json")
	g.Succeeded("weird", "not-json")

	if _, _, blocked := g.Blocked("weird", "not-json"); !blocked {
		t.Fatal("非 JSON 参数相同也应当被判定为循环")
	}
}

// TestLoopObservationGivesWayOut 拦截文案必须给出路。
//
// 只说"不许重复调用"的话，弱模型会不知所措继续空转 ——
// 这条断言是在保护那个产品决定，而不是在保护措辞。
func TestLoopObservationGivesWayOut(t *testing.T) {
	msg := loopObservation("list_files", `{"path":"."}`, 3, false)

	for _, want := range []string{"list_files", "3 次", "换一个参数", "换一个工具", "作答"} {
		if !strings.Contains(msg, want) {
			t.Errorf("拦截文案里应当包含 %q：\n%s", want, msg)
		}
	}
}

// TestLoopObservationTruncatesLongArgs 超长参数要被截断，不能整段塞回上下文。
func TestLoopObservationTruncatesLongArgs(t *testing.T) {
	long := `{"content":"` + strings.Repeat("x", 5000) + `"}`
	msg := loopObservation("write_file", long, 4, false)

	if len(msg) > 400 {
		t.Fatalf("文案应当被截断，实际 %d 字符", len(msg))
	}
	if !strings.Contains(msg, "…") {
		t.Error("截断应当有省略号，让模型知道这里被截了")
	}
}

// TestLoopGuardBlocksFailedLoop 失败循环同样要被拦下来。
//
// 这一条是被追问出来的：原来的设计只计成功，理由是"失败后重试同样的参数是
// 合理的"。理由站不住 —— 瞬时错误的重试已经在**工具内部**做完了
// （toolMaxAttempts 默认 2），失败能冒到模型面前说明重试已经用尽。
//
// 只计成功的后果：失败循环只能靠预算兜底，结局是 "exceeds max steps"，
// 和成功循环一样烂，只是换了个失败方式。
func TestLoopGuardBlocksFailedLoop(t *testing.T) {
	g := NewLoopGuard(3)

	for i := 1; i <= 3; i++ {
		if _, _, blocked := g.Blocked("flaky", `{"x":1}`); blocked {
			t.Fatalf("第 %d 次不该被拦", i)
		}
		g.Failed("flaky", `{"x":1}`)
	}

	attempts, allFailed, blocked := g.Blocked("flaky", `{"x":1}`)
	if !blocked {
		t.Fatal("连续失败 3 次后应当被拦")
	}
	if !allFailed {
		t.Error("三次全失败，应当标记为 allFailed，好给出不同的措辞")
	}
	if attempts != 3 {
		t.Fatalf("应报出 3 次尝试，实际 %d", attempts)
	}
}

// TestLoopGuardMixedOutcomeNotAllFailed 成功失败交替时不算"全失败"。
//
// 措辞要准：告诉模型"结果不会再变"和"同样的参数试了也没用"是两句不同的话，
// 用错会把它带偏。
func TestLoopGuardMixedOutcomeNotAllFailed(t *testing.T) {
	g := NewLoopGuard(3)

	g.Succeeded("mixed", `{"x":1}`)
	g.Failed("mixed", `{"x":1}`)
	g.Succeeded("mixed", `{"x":1}`)

	_, allFailed, blocked := g.Blocked("mixed", `{"x":1}`)
	if !blocked {
		t.Fatal("3 次调用后应当被拦")
	}
	if allFailed {
		t.Error("有成功过就不该算 allFailed")
	}
}

// TestLoopObservationDistinguishesFailureLoop 两种循环必须给出不同的话。
func TestLoopObservationDistinguishesFailureLoop(t *testing.T) {
	okMsg := loopObservation("t", `{}`, 3, false)
	failMsg := loopObservation("t", `{}`, 3, true)

	if !strings.Contains(okMsg, "成功调用过") {
		t.Errorf("成功循环的措辞不对：%s", okMsg)
	}
	if !strings.Contains(failMsg, "失败") {
		t.Errorf("失败循环应当说明是失败：%s", failMsg)
	}
	if okMsg == failMsg {
		t.Error("两种循环必须给出不同的措辞，否则模型会误以为失败只是还没成功")
	}
	// 失败循环也要给出路，不能只说"不许重试"
	if !strings.Contains(failMsg, "换一个参数") {
		t.Errorf("失败循环也要给出路：%s", failMsg)
	}
}

// invoke 走真实的工具执行路径（specTool.InvokableRun）。
//
// 直接构造 specTool 而不是 spec.BaseTool()：后者返回的是 eino 的 BaseTool
// 接口，不含 InvokableRun；要拿到它得再做一次类型断言，绕一圈还是回到同一个
// 类型。测试与实现同包，直接构造更清楚。
func invoke(t *testing.T, spec ToolSpec, ctx context.Context, args string) (string, error) {
	t.Helper()
	return (&specTool{spec: spec}).InvokableRun(ctx, args)
}

// TestInvokableRunBlocksRepeatedCalls 端到端：走**真实的工具执行路径**验证拦截。
//
// 这是本文件里最重要的一条 —— 前面那些测的是守卫自己的逻辑，
// 这条测的是"它有没有真的接进执行路径"。两者都过才算功能可用：
// 一个逻辑正确但没被调用的守卫，和一个不存在的守卫没有区别。
func TestInvokableRunBlocksRepeatedCalls(t *testing.T) {
	executed := 0
	spec := ToolSpec{
		Name:        "echo",
		Description: "回声",
		Handler: func(context.Context, string) (string, error) {
			executed++
			return "ok", nil
		},
	}

	ctx := WithLoopGuard(context.Background(), NewLoopGuard(3))
	ctx = WithWarnings(ctx, NewWarnCollector())

	// 前 3 次正常执行
	for i := 1; i <= 3; i++ {
		out, err := invoke(t, spec, ctx, `{"path":"."}`)
		if err != nil {
			t.Fatalf("第 %d 次不该报错: %v", i, err)
		}
		if strings.Contains(out, "重复调用") {
			t.Fatalf("第 %d 次不该被拦，输出：%s", i, out)
		}
	}

	// 第 4 次：被拦，且**没有真正执行**
	out, err := invoke(t, spec, ctx, `{"path":"."}`)
	if err != nil {
		t.Fatalf("拦截必须是回填 observation，不是返回 error；实际 err=%v", err)
	}
	if !strings.Contains(out, "重复调用") {
		t.Fatalf("第 4 次应当被拦，实际输出：%s", out)
	}
	if executed != 3 {
		t.Fatalf("被拦的那次不该真正执行工具：期望 3 次，实际 %d", executed)
	}

	// 用户也要收到一条告警 —— 他有权知道 Agent 绕了个弯
	ws := WarningsFrom(ctx)
	if len(ws) == 0 {
		t.Fatal("拦截应当同时给用户一条告警")
	}
	if !strings.Contains(ws[0].Message, "重复") {
		t.Errorf("告警文案不对：%s", ws[0].Message)
	}
}

// TestInvokableRunAllowsDistinctArgs 参数不同就照常执行 —— 拦截不能误伤正常使用。
func TestInvokableRunAllowsDistinctArgs(t *testing.T) {
	executed := 0
	spec := ToolSpec{
		Name:        "read_file",
		Description: "读文件",
		Handler: func(context.Context, string) (string, error) {
			executed++
			return "ok", nil
		},
	}

	ctx := WithLoopGuard(context.Background(), NewLoopGuard(2))
	for _, args := range []string{`{"path":"a"}`, `{"path":"b"}`, `{"path":"c"}`, `{"path":"d"}`} {
		out, err := invoke(t, spec, ctx, args)
		if err != nil || strings.Contains(out, "重复调用") {
			t.Fatalf("读不同文件是正常行为，不该被拦：args=%s out=%s err=%v", args, out, err)
		}
	}
	if executed != 4 {
		t.Fatalf("4 次不同参数都该真正执行，实际 %d", executed)
	}
}
