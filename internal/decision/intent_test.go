package decision

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"deeptalk/internal/infra/config"

	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

func TestMain(m *testing.M) {
	if _, file, _, ok := runtime.Caller(0); ok {
		root := filepath.Dir(filepath.Dir(filepath.Dir(file)))
		_ = os.Setenv("DEEPTALK_CONFIG", filepath.Join(root, "config", "config.toml.example"))
	}
	os.Exit(m.Run())
}

// ---------------- 规则层：纯本地，零成本 ----------------

func TestRuleIntent(t *testing.T) {
	cases := []struct {
		q      string
		want   Intent
		reason string
	}{
		// 总结类
		{"总结一下这份文档", IntentSummary, "命中总结词"},
		{"请概括全文", IntentSummary, "命中总结词"},
		{"这份制度讲了什么", IntentSummary, "命中「讲了什么」"},
		{"帮我梳理一下内容", IntentSummary, "命中「梳理一下」"},

		// 寒暄类
		{"你好", IntentChat, "短句寒暄"},
		{"谢谢", IntentChat, "短句寒暄"},
		{"hello", IntentChat, "英文整词命中"},
		{"hi", IntentChat, "英文整词命中"},

		// 关键修正 1：英文子串不该误判（原来 Contains("hi") 会命中 this/which/machine）
		{"This policy is about leave", IntentQuestion, "hi 是 this 的子串，必须整词匹配"},
		{"which one is right", IntentQuestion, "hi 是 which 的子串"},
		{"machine learning policy", IntentQuestion, "hi 是 machine 的子串"},

		// 关键修正 2：指代词必须判为提问（原来"那它呢"因为太短被判闲聊）
		{"那它呢", IntentQuestion, "含指代词"},
		{"这个怎么申请", IntentQuestion, "含指代词"},
		{"刚才说的那个呢", IntentQuestion, "含指代词"},
		{"上述规定适用吗", IntentQuestion, "含指代词"},

		// 关键修正 3：原来"怎么样"会把正常提问判成闲聊
		{"年假怎么样申请", IntentQuestion, "含指代词外的正常提问，不应判闲聊"},

		// 弱总结词 + 具体疑问要素 → 其实是要一个具体答案，走检索而不是整篇概括
		{"总结一下员工放假几天", IntentQuestion, "具体问题优先"},
		{"总结一下年假有多少天", IntentQuestion, "具体问题优先"},
		{"总结一下这份文档", IntentSummary, "纯总结诉求"},
		{"请概括全文", IntentSummary, "强总结词"},

		// 元问题守卫：问助手自身 → 闲聊（不能因为带"吗"就当成文档提问）
		{"你是RAG模型吗", IntentChat, "问助手自身"},
		{"你是什么模型", IntentChat, "问助手自身"},
		{"你会画画吗", IntentChat, "问助手能力"},
		{"你能写代码吗", IntentChat, "问助手能力"},
		// 元问题守卫的反例：交代任务时不能判成闲聊
		{"你能帮我查一下年假吗", IntentQuestion, "是任务请求，不是元问题"},
		{"你能告诉我报销流程吗", IntentQuestion, "是任务请求"},

		// 默认：正常提问
		{"正式员工请假需要提前多久", IntentQuestion, "默认提问"},
		{"报销单笔超过 500 元谁签字", IntentQuestion, "默认提问"},
	}

	for _, c := range cases {
		got, _ := ruleIntent(c.q)
		if got.Intent != c.want {
			t.Errorf("ruleIntent(%q) = %s, want %s（%s）", c.q, got.Intent, c.want, c.reason)
		}
	}
}

// 模糊地带必须标记为"不自信"，才会走 LLM 兜底
func TestRuleIntentConfidence(t *testing.T) {
	// 高置信度：明确命中/明确默认
	for _, q := range []string{"你好", "总结一下全文", "年假有多少天"} {
		if _, confident := ruleIntent(q); !confident {
			t.Errorf("%q 应该被判为高置信度", q)
		}
	}
	// 低置信度：命中寒暄词但句子很长（可能是"你好，帮我查年假"）
	for _, q := range []string{"你好我想了解一下年假制度的相关规定", "谢谢你的说明我们稍后再讨论细节"} {
		if _, confident := ruleIntent(q); confident {
			t.Errorf("%q 应该被判为低置信度（需 LLM 兜底）", q)
		}
	}
	// 口语附和词现在能判对（高置信度闲聊）
	if res, confident := ruleIntent("嗯"); !confident || res.Intent != IntentChat {
		t.Errorf("「嗯」应判为闲聊且高置信度，实际 intent=%s confident=%v", res.Intent, confident)
	}

	// 低置信度：极短、无词表命中、无实义词（"在么"）
	if _, confident := ruleIntent("在么"); confident {
		t.Error("「在么」应被判为低置信度（需 LLM 兜底）")
	}
}

// ---------------- LLM 兜底层：用假模型验证链路（不需要网络/密钥） ----------------

type fakeIntentLLM struct {
	intent string
	calls  int
	fail   bool
	noTool bool // 模拟模型不按格式调工具
}

func (f *fakeIntentLLM) Generate(ctx context.Context, in []*schema.Message, opts ...einomodel.Option) (*schema.Message, error) {
	f.calls++
	if f.fail {
		return nil, context.DeadlineExceeded
	}
	msg := &schema.Message{Role: schema.Assistant}
	if f.noTool {
		msg.Content = "我猜这是闲聊吧"
		return msg, nil
	}
	msg.ToolCalls = []schema.ToolCall{{
		ID:   "call_1",
		Type: "function",
		Function: schema.FunctionCall{
			Name:      "set_intent",
			Arguments: `{"intent":"` + f.intent + `","reason":"假模型判定"}`,
		},
	}}
	return msg, nil
}

func (f *fakeIntentLLM) WithTools(tools []*schema.ToolInfo) (einomodel.ToolCallingChatModel, error) {
	return f, nil
}

// Stream 假实现（意图识别只用 Generate，这里满足接口即可）
func (f *fakeIntentLLM) Stream(ctx context.Context, in []*schema.Message, opts ...einomodel.Option) (*schema.StreamReader[*schema.Message], error) {
	return nil, context.Canceled
}

// 低置信度 → 交给 LLM 判；LLM 结果被采纳
func TestClassifyIntentLLMFallback(t *testing.T) {
	resetIntentCache()
	cfg := config.GetConfig()
	cfg.IntentConfig.LLMFallback = true
	cfg.IntentConfig.CacheTTLSeconds = 1
	t.Cleanup(func() { cfg.IntentConfig = config.IntentConfig{} })

	llm := &fakeIntentLLM{intent: "question"}
	q := "你好我想了解一下年假制度的相关规定" // 规则层低置信度

	res := ClassifyIntent(context.Background(), q, llm)
	if res.Layer != LayerLLM {
		t.Fatalf("应走 LLM 兜底，实际 layer=%s（%s）", res.Layer, res.Reason)
	}
	if res.Intent != IntentQuestion {
		t.Fatalf("应采用 LLM 判定结果，实际 %s", res.Intent)
	}
	if llm.calls != 1 {
		t.Fatalf("兜底模型应被调用 1 次，实际 %d 次", llm.calls)
	}

	// 第二次同问题 → 命中缓存，不再调用模型
	res2 := ClassifyIntent(context.Background(), q, llm)
	if llm.calls != 1 {
		t.Fatalf("相同问题应命中缓存，实际调用 %d 次", llm.calls)
	}
	if res2.Intent != res.Intent {
		t.Fatalf("缓存结果不一致: %s vs %s", res2.Intent, res.Intent)
	}
}

// 高置信度问题不该浪费一次 LLM 调用（这是"LLM 占比可控"的关键）
func TestClassifyIntentSkipsLLMWhenConfident(t *testing.T) {
	resetIntentCache()
	cfg := config.GetConfig()
	cfg.IntentConfig.LLMFallback = true
	t.Cleanup(func() { cfg.IntentConfig = config.IntentConfig{} })

	llm := &fakeIntentLLM{intent: "chat"}
	res := ClassifyIntent(context.Background(), "正式员工请假需要提前多久", llm)
	if res.Layer != LayerRule {
		t.Fatalf("高置信度应直接由规则层给出，实际 layer=%s", res.Layer)
	}
	if llm.calls != 0 {
		t.Fatalf("高置信度不应调用兜底模型，实际调用 %d 次", llm.calls)
	}
}

// 兜底失败（模型报错 / 不按格式调工具）→ 安全默认，绝不猜
func TestClassifyIntentFailSafe(t *testing.T) {
	resetIntentCache()
	cfg := config.GetConfig()
	cfg.IntentConfig.LLMFallback = true
	t.Cleanup(func() { cfg.IntentConfig = config.IntentConfig{} })

	q := "你好我想了解一下年假制度的相关规定"

	if res := ClassifyIntent(context.Background(), q, &fakeIntentLLM{fail: true}); res.Intent != IntentQuestion || res.Layer != LayerDefault {
		t.Fatalf("模型报错时应回退安全默认，实际 intent=%s layer=%s", res.Intent, res.Layer)
	}
	if res := ClassifyIntent(context.Background(), q, &fakeIntentLLM{noTool: true}); res.Intent != IntentQuestion || res.Layer != LayerDefault {
		t.Fatalf("模型不调工具时应回退安全默认，实际 intent=%s layer=%s", res.Intent, res.Layer)
	}
	if res := ClassifyIntent(context.Background(), q, nil); res.Intent != IntentQuestion || res.Layer != LayerDefault {
		t.Fatalf("未配置模型时应回退安全默认，实际 intent=%s layer=%s", res.Intent, res.Layer)
	}
}

// 关闭 llmFallback 时纯规则运行（零成本模式）
func TestClassifyIntentRuleOnlyMode(t *testing.T) {
	resetIntentCache()
	cfg := config.GetConfig()
	cfg.IntentConfig.LLMFallback = false
	t.Cleanup(func() { cfg.IntentConfig = config.IntentConfig{} })

	llm := &fakeIntentLLM{intent: "chat"}
	ClassifyIntent(context.Background(), "你好我想了解一下年假制度的相关规定", llm)
	if llm.calls != 0 {
		t.Fatalf("关闭兜底后不应调用模型，实际 %d 次", llm.calls)
	}
}
