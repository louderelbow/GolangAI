package trace

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"deeptalk/internal/infra/metrics"
)

func TestSpanRecordsDurationAndStatus(t *testing.T) {
	rec := New("s1", "u1", "m1", "6")

	sp := rec.Begin(KindTool, "read_file")
	time.Sleep(2 * time.Millisecond)
	sp.End(StatusOK, "12 行")

	got := rec.Snapshot(StatusOK)
	if len(got.Steps) != 1 {
		t.Fatalf("应当记下 1 步，实际 %d", len(got.Steps))
	}
	s := got.Steps[0]
	if s.Kind != KindTool || s.Name != "read_file" || s.Status != StatusOK || s.Detail != "12 行" {
		t.Fatalf("步骤内容不对: %+v", s)
	}
	if s.Duration < 2*time.Millisecond {
		t.Errorf("耗时应至少 2ms，实际 %v", s.Duration)
	}
}

// TestEndIsIdempotent defer 与显式调用并存时不能记两条。
func TestEndIsIdempotent(t *testing.T) {
	rec := New("s1", "u1", "m1", "6")

	sp := rec.Begin(KindModel, "generate")
	sp.EndOK("先结束")
	sp.End(StatusError, "再结束一次")

	if n := rec.Count(KindModel); n != 1 {
		t.Fatalf("重复 End 只应产生 1 条，实际 %d", n)
	}
}

// TestStepsSortByStart 轨迹必须按**开始**时间读，而不是结束时间。
//
// 嵌套步骤（整轮包着模型调用）如果按结束时间排，turn 会排在最后，
// 读起来就像"先调了模型，最后才开始的这一轮"。
func TestStepsSortByStart(t *testing.T) {
	rec := New("s1", "u1", "m1", "6")

	outer := rec.Begin(KindTurn, "turn")
	time.Sleep(time.Millisecond)
	inner := rec.Begin(KindModel, "generate")
	inner.EndOK("")
	time.Sleep(time.Millisecond)
	outer.End(StatusOK, "")

	steps := rec.Snapshot(StatusOK).Steps
	if len(steps) != 2 {
		t.Fatalf("应有 2 步，实际 %d", len(steps))
	}
	if steps[0].Kind != KindTurn || steps[1].Kind != KindModel {
		t.Fatalf("应先 turn 后 model，实际 %s, %s", steps[0].Kind, steps[1].Kind)
	}
}

// TestNilRecorderIsSafe 没有记录器（例如单测）时必须安静退化成什么都不记。
//
// 这条保证了调用点不必到处判空——一旦需要判空，就一定会有人忘了判。
func TestNilRecorderIsSafe(t *testing.T) {
	var rec *Recorder
	sp := rec.Begin(KindTool, "x")
	sp.EndOK("") // 不应 panic
	rec.Record(KindTool, "x", StatusOK, time.Now(), "")
	if rec.Count(KindTool) != 0 || rec.ID() != "" {
		t.Fatal("nil recorder 不应产生任何记录")
	}

	// 没有记录器的 ctx 同理
	sp2 := Begin(context.Background(), KindTool, "y")
	sp2.EndOK("")
}

func TestStatusOfClassifies(t *testing.T) {
	cases := []struct {
		err  error
		want Status
	}{
		{nil, StatusOK},
		{context.DeadlineExceeded, StatusTimeout},
		{context.Canceled, StatusSkipped},
		{errors.New("boom"), StatusError},
	}
	for _, c := range cases {
		if got := StatusOf(c.err); got != c.want {
			t.Errorf("StatusOf(%v) = %s，期望 %s", c.err, got, c.want)
		}
	}
}

// TestEndErrWrapsCause 包装过的超时也要认出来。
func TestEndErrWrapsCause(t *testing.T) {
	rec := New("s1", "u1", "m1", "6")
	rec.Begin(KindTool, "x").EndErr(
		errors.Join(errors.New("调用失败"), context.DeadlineExceeded), "")

	if got := rec.Snapshot(StatusOK).Steps[0].Status; got != StatusTimeout {
		t.Fatalf("包装过的超时应识别为 timeout，实际 %s", got)
	}
}

// TestFlushExposesMetrics 轨迹必须真的变成 Prometheus 序列。
//
// 用真实 /metrics 出口断言，而不是去查内部结构：指标的名字、标签、
// 分桶就是对外契约，只测内部变量等于没测契约。
func TestFlushExposesMetrics(t *testing.T) {
	metrics.RegisterHelp()

	rec := New("s1", "u1", "deepseek-chat", "6")
	rec.Begin(KindTool, "read_file").EndOK("ok")
	rec.Begin(KindTool, "read_file").End(StatusRejected, "越界")
	rec.Begin(KindModel, "generate").EndOK("给出回答")
	rec.Begin(KindClarify, "asked").EndOK("")

	Flush(rec.Snapshot(StatusOK))

	body := scrape(t)
	for _, want := range []string{
		`deeptalk_agent_turns_total{`,
		`deeptalk_agent_tool_calls_total{status="ok",tool="read_file"}`,
		`deeptalk_agent_tool_calls_total{status="rejected",tool="read_file"}`,
		`deeptalk_agent_model_calls_total{`,
		`deeptalk_agent_clarify_total{outcome="asked"}`,
		`deeptalk_agent_trace_spans_total{kind="tool"}`,
		`deeptalk_agent_steps_per_turn_bucket`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics 里缺少 %q", want)
		}
	}
}

// TestFlushCountsStepsNotToolCalls 步数取自模型调用次数，而不是工具调用次数。
//
// 这是"Agent 效率"的核心口径：一个只想要不想做的 Agent 会有很少的工具调用，
// 若用工具调用数当步数，那种空转在指标上完全看不出来。
func TestFlushCountsStepsNotToolCalls(t *testing.T) {
	metrics.RegisterHelp()

	rec := New("s1", "u1", "steps-probe-model", "6")
	for i := 0; i < 3; i++ {
		rec.Begin(KindModel, "generate").EndOK("")
	}
	Flush(rec.Snapshot(StatusOK))

	body := scrape(t)
	// 一次观测、观测值为 3：直方图的 _count 数的是**观测次数**（1），
	// 观测到的步数在 _sum 里（3）。别把 _count 当成步数。
	if !strings.Contains(body, `deeptalk_agent_steps_per_turn_sum{model="steps-probe-model"} 3`) {
		t.Errorf("步数之和应为 3（模型调用次数）\n%s", grepLines(body, "steps_per_turn"))
	}
}

func scrape(t *testing.T) string {
	t.Helper()
	w := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	return w.Body.String()
}

func grepLines(body, needle string) string {
	var sb strings.Builder
	for _, line := range strings.Split(body, "\n") {
		if strings.Contains(line, needle) {
			sb.WriteString(line + "\n")
		}
	}
	return sb.String()
}
