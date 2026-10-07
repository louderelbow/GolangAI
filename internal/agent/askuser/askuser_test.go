package askuser

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func opt(id, label string) Option { return Option{ID: id, Label: label} }

// ==================== 选项清洗 ====================

func TestNormalizeDropsEmptyAndDuplicateOptions(t *testing.T) {
	// 模型经常多给一个空选项，或者把同一个选项写两遍；
	// 直接透给前端会渲染出一排点了没反应的按钮。
	r := Request{
		Question: "  你要请哪种假？  ",
		Options: []Option{
			opt("", "年假"),
			opt("", "  "), // 空 label
			opt("", "年假"), // 重复
			opt("", "病假"),
		},
	}
	r.Normalize()

	if r.Question != "你要请哪种假？" {
		t.Errorf("问题应被 trim，实际 %q", r.Question)
	}
	if len(r.Options) != 2 {
		t.Fatalf("应剩下 2 个有效选项，实际 %d: %+v", len(r.Options), r.Options)
	}
	if r.Options[0].Label != "年假" || r.Options[1].Label != "病假" {
		t.Errorf("选项内容不对: %+v", r.Options)
	}
}

func TestNormalizeCapsOptionsAtMax(t *testing.T) {
	r := Request{Question: "选一个"}
	for i := 0; i < 12; i++ {
		r.Options = append(r.Options, opt("", string(rune('a'+i))))
	}
	r.Normalize()

	if len(r.Options) != MaxOptions {
		t.Fatalf("选项应被截断到 %d 个，实际 %d", MaxOptions, len(r.Options))
	}
}

func TestNormalizeAssignsIDWhenMissing(t *testing.T) {
	r := Request{Question: "选一个", Options: []Option{opt("", "甲"), opt("", "乙")}}
	r.Normalize()

	if r.Options[0].ID == "" || r.Options[1].ID == "" {
		t.Fatalf("缺 id 时应补上，实际 %+v", r.Options)
	}
	if r.Options[0].ID == r.Options[1].ID {
		t.Fatalf("补出来的 id 不能相同: %+v", r.Options)
	}
}

func TestValidRequiresQuestionAndTwoOptions(t *testing.T) {
	cases := []struct {
		name string
		req  Request
		want bool
	}{
		{"正常", Request{Question: "问", Options: []Option{opt("1", "甲"), opt("2", "乙")}}, true},
		{"没问题", Request{Options: []Option{opt("1", "甲"), opt("2", "乙")}}, false},
		{"只有一个选项", Request{Question: "问", Options: []Option{opt("1", "甲")}}, false},
		{"没有选项", Request{Question: "问"}, false},
	}
	for _, c := range cases {
		if got := c.req.Valid(); got != c.want {
			t.Errorf("%s: Valid()=%v, want %v", c.name, got, c.want)
		}
	}
}

func TestRequestAsTextListsOptions(t *testing.T) {
	r := Request{Question: "你要请哪种假？", Options: []Option{opt("1", "年假"), opt("2", "病假")}}
	text := r.AsText()

	// 这段文本会作为助手发言落库，用户回看历史时要能读懂
	if !strings.Contains(text, "你要请哪种假？") {
		t.Errorf("应包含问题，实际 %q", text)
	}
	if !strings.Contains(text, "年假") || !strings.Contains(text, "病假") {
		t.Errorf("应包含选项，实际 %q", text)
	}
	if strings.Contains(text, `"id"`) || strings.Contains(text, "{") {
		t.Errorf("落库的应当是自然语言而不是 JSON，实际 %q", text)
	}
}

// ==================== 收集器 ====================

func TestCollectorTakeClears(t *testing.T) {
	c := NewCollector()
	c.Set(Request{Question: "问", Options: []Option{opt("1", "甲"), opt("2", "乙")}})

	got, ok := c.Take()
	if !ok || got.Question != "问" {
		t.Fatalf("应取到澄清请求，实际 ok=%v %+v", ok, got)
	}
	if _, ok := c.Take(); ok {
		t.Fatal("取走后应清空——否则下一次普通回答会被误判成提问")
	}
}

// TestCollectorKeepsFirst 同一轮问两次只保留第一次：
// 模型一旦决定问，后面再问只会让用户困惑。
func TestCollectorKeepsFirst(t *testing.T) {
	c := NewCollector()
	c.Set(Request{Question: "第一个", Options: []Option{opt("1", "甲"), opt("2", "乙")}})
	c.Set(Request{Question: "第二个", Options: []Option{opt("1", "丙"), opt("2", "丁")}})

	got, _ := c.Take()
	if got.Question != "第一个" {
		t.Fatalf("应保留第一次提问，实际 %q", got.Question)
	}
}

func TestCollectorIgnoresInvalidRequest(t *testing.T) {
	c := NewCollector()
	c.Set(Request{Question: "只有一个选项", Options: []Option{opt("1", "甲")}})

	if _, ok := c.Take(); ok {
		t.Fatal("选项不足 2 个的请求不该被记下来")
	}
}

// TestCollectorIsPerTurn 两个收集器互不干扰（对应两轮并发会话）。
func TestCollectorIsPerTurn(t *testing.T) {
	ctxA := WithCollector(context.Background(), NewCollector())
	ctxB := WithCollector(context.Background(), NewCollector())

	ca, _ := FromContext(ctxA)
	cb, _ := FromContext(ctxB)
	ca.Set(Request{Question: "A 的问题", Options: []Option{opt("1", "甲"), opt("2", "乙")}})

	if _, ok := cb.Take(); ok {
		t.Fatal("B 轮不该看到 A 轮的提问")
	}
	if got, ok := ca.Take(); !ok || got.Question != "A 的问题" {
		t.Fatalf("A 轮应取到自己的提问，实际 %+v", got)
	}
}

func TestFromContextWithoutCollector(t *testing.T) {
	if _, ok := FromContext(context.Background()); ok {
		t.Fatal("没放收集器时不该返回 ok——非 Agent 模型依赖这个默认行为")
	}
}

// ==================== 工具 ====================

func runTool(t *testing.T, ctx context.Context, args any) (string, error) {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("序列化参数失败: %v", err)
	}
	return Tool().Handler(ctx, string(raw))
}

// TestToolRequestsClarification 正常路径：写进收集器 + 用哨兵错误中断本轮。
func TestToolRequestsClarification(t *testing.T) {
	collector := NewCollector()
	ctx := WithCollector(context.Background(), collector)

	out, err := runTool(t, ctx, map[string]any{
		"question": "你要请哪种假？",
		"reason":   "三种假的规则不同",
		"options": []map[string]string{
			{"id": "annual", "label": "年假"},
			{"id": "sick", "label": "病假"},
		},
	})

	// 必须用哨兵错误中断本轮：eino 的 ReAct 只在工具报错时才停止循环。
	// 换成返回一句普通观察，模型会继续编一个答案出来。
	if !errors.Is(err, ErrRequested) {
		t.Fatalf("应返回 ErrRequested 以中断本轮，实际 err=%v", err)
	}

	got, ok := collector.Take()
	if !ok {
		t.Fatal("澄清请求应已写进收集器")
	}
	if got.Question != "你要请哪种假？" || len(got.Options) != 2 {
		t.Fatalf("收集到的请求不对: %+v", got)
	}
	if out != "" {
		t.Errorf("中断时不该再返回观察文本，实际 %q", out)
	}
}

// TestToolRejectsTooFewOptions 参数不合法时**不能**中断整轮：
// 模型把选项写少了是它的问题，不该让用户什么回答都拿不到。
func TestToolRejectsTooFewOptions(t *testing.T) {
	collector := NewCollector()
	ctx := WithCollector(context.Background(), collector)

	out, err := runTool(t, ctx, map[string]any{
		"question": "你要请哪种假？",
		"options":  []map[string]string{{"label": "年假"}},
	})

	if err != nil {
		t.Fatalf("参数不合法不该报错中断本轮，实际 %v", err)
	}
	if !strings.Contains(out, "2~6 个") {
		t.Errorf("应回填一句可操作的观察，实际 %q", out)
	}
	if _, ok := collector.Take(); ok {
		t.Fatal("不合法的请求不该被记下来")
	}
}

// TestToolWithoutCollector 非 Agent 路径（没放收集器）时应优雅退化为普通观察。
func TestToolWithoutCollector(t *testing.T) {
	out, err := runTool(t, context.Background(), map[string]any{
		"question": "问",
		"options":  []map[string]string{{"label": "甲"}, {"label": "乙"}},
	})

	if err != nil {
		t.Fatalf("没有收集器时不该报错，实际 %v", err)
	}
	if !strings.Contains(out, "直接回答") {
		t.Errorf("应告诉模型直接回答，实际 %q", out)
	}
}

// TestToolMalformedArgsDegradeGracefully 参数格式坏掉时也不能中断整轮：
// 那是模型生成的问题，不该让用户拿到"模型运行失败"。
func TestToolMalformedArgsDegradeGracefully(t *testing.T) {
	out, err := Tool().Handler(context.Background(), "这不是 JSON")
	if err != nil {
		t.Fatalf("参数格式错误不该中断整轮，实际 err=%v", err)
	}
	if !strings.Contains(out, "直接回答") {
		t.Errorf("应回填一句可操作的观察，实际 %q", out)
	}
}

func TestToolSpecIsNotRetried(t *testing.T) {
	spec := Tool()
	if spec.Name != ToolName {
		t.Errorf("工具名应为 %s，实际 %s", ToolName, spec.Name)
	}
	// 重试会变成问用户两次
	if spec.Idempotent {
		t.Error("ask_user 不该标记为幂等")
	}
	if spec.RetryPolicy.MaxAttempts != 1 {
		t.Errorf("不该自动重试，实际 MaxAttempts=%d", spec.RetryPolicy.MaxAttempts)
	}
	// 描述是提示词工程的重点：必须写清"什么时候不要调用"
	for _, want := range []string{"不要调用", "2~6 个", "本轮立即结束"} {
		if !strings.Contains(spec.Description, want) {
			t.Errorf("工具描述缺少 %q —— 缺少这条约束会导致模型滥用提问", want)
		}
	}
}
