package guard

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/prometheus/client_golang/prometheus"
)

// ==================== 测试替身 ====================

type stubModel struct {
	name   string
	calls  int32
	failN  int32 // 前 N 次调用失败，之后成功
	reason string
}

func (m *stubModel) WithTools([]*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return m, nil
}

func (m *stubModel) Generate(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	n := atomic.AddInt32(&m.calls, 1)
	if n <= m.failN {
		if m.reason == "" {
			m.reason = "upstream unavailable"
		}
		return nil, errors.New(m.reason)
	}
	return &schema.Message{Role: schema.Assistant, Content: "from-" + m.name}, nil
}

func (m *stubModel) Stream(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	msg, err := m.Generate(ctx, input, opts...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{msg}), nil
}

// counterValue 从默认 registry 里读回某个序列的值，用来断言指标真的被记录。
func counterValue(t *testing.T, name string, want map[string]string) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather 失败: %v", err)
	}
	for _, mf := range families {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			got := map[string]string{}
			for _, lp := range m.GetLabel() {
				got[lp.GetName()] = lp.GetValue()
			}
			match := true
			for k, v := range want {
				if got[k] != v {
					match = false
					break
				}
			}
			if match {
				if c := m.GetCounter(); c != nil {
					return c.GetValue()
				}
			}
		}
	}
	return 0
}

// ==================== 用例 ====================

// TestFallbackSwitchesToBackupModel 验收：主模型失败时切换备用模型并记指标。
func TestFallbackSwitchesToBackupModel(t *testing.T) {
	primary := &stubModel{name: "primary", failN: 100} // 恒失败
	backup := &stubModel{name: "backup"}

	before := counterValue(t, "deeptalk_agent_degraded_total", map[string]string{"level": "fallback_model"})

	chain := NewFallbackModel(primary, []model.ToolCallingChatModel{backup}, []string{"backup"})
	msg, err := chain.Generate(context.Background(), []*schema.Message{{Role: schema.User, Content: "hi"}})
	if err != nil {
		t.Fatalf("备用模型可用时不应报错: %v", err)
	}
	if msg.Content != "from-backup" {
		t.Fatalf("应由备用模型作答，实际: %q", msg.Content)
	}

	after := counterValue(t, "deeptalk_agent_degraded_total", map[string]string{"level": "fallback_model"})
	if after <= before {
		t.Fatalf("应记录 fallback_model 降级指标: before=%v after=%v", before, after)
	}
}

// TestFallbackRetrySucceedsWithoutSwitching 原模型第二次成功时，
// 只记 retry，不切备用模型。
func TestFallbackRetrySucceedsWithoutSwitching(t *testing.T) {
	primary := &stubModel{name: "primary", failN: 1} // 第一次失败，之后成功
	backup := &stubModel{name: "backup"}

	retryBefore := counterValue(t, "deeptalk_agent_degraded_total", map[string]string{"level": "retry"})
	fbBefore := counterValue(t, "deeptalk_agent_degraded_total", map[string]string{"level": "fallback_model"})

	chain := NewFallbackModel(primary, []model.ToolCallingChatModel{backup}, []string{"backup"})
	msg, err := chain.Generate(context.Background(), []*schema.Message{{Role: schema.User, Content: "hi"}})
	if err != nil {
		t.Fatalf("重试后应成功: %v", err)
	}
	if msg.Content != "from-primary" {
		t.Fatalf("应仍由主模型作答，实际: %q", msg.Content)
	}
	if got := atomic.LoadInt32(&backup.calls); got != 0 {
		t.Fatalf("主模型重试成功时不应调用备用模型，实际调用 %d 次", got)
	}

	if after := counterValue(t, "deeptalk_agent_degraded_total", map[string]string{"level": "retry"}); after <= retryBefore {
		t.Fatalf("应记录 retry 指标: before=%v after=%v", retryBefore, after)
	}
	if after := counterValue(t, "deeptalk_agent_degraded_total", map[string]string{"level": "fallback_model"}); after != fbBefore {
		t.Fatalf("不应记录 fallback_model: before=%v after=%v", fbBefore, after)
	}
}

// TestAllModelsFailReturnsSentinel 验收：主备都失败时返回可识别错误，不 panic。
func TestAllModelsFailReturnsSentinel(t *testing.T) {
	primary := &stubModel{name: "primary", failN: 100}
	backup := &stubModel{name: "backup", failN: 100}

	errBefore := counterValue(t, "deeptalk_agent_degraded_total", map[string]string{"level": "error"})

	chain := NewFallbackModel(primary, []model.ToolCallingChatModel{backup}, []string{"backup"})
	msg, err := chain.Generate(context.Background(), []*schema.Message{{Role: schema.User, Content: "hi"}})
	if err == nil {
		t.Fatal("全部失败时应返回错误")
	}
	if msg != nil {
		t.Fatalf("失败时不应返回消息，实际: %+v", msg)
	}
	if !errors.Is(err, ErrAllModelsFailed) {
		t.Fatalf("应返回 ErrAllModelsFailed，实际: %v", err)
	}
	if !strings.Contains(err.Error(), "upstream unavailable") {
		t.Fatalf("错误信息应保留上游原因，实际: %v", err)
	}

	if after := counterValue(t, "deeptalk_agent_degraded_total", map[string]string{"level": "error"}); after <= errBefore {
		t.Fatalf("应记录 error 降级指标: before=%v after=%v", errBefore, after)
	}
}

// TestFallbackSkipsWhenContextCancelled 调用方主动取消时不应切换模型：
// 请求已经放弃了，换模型只会白烧一次调用。
func TestFallbackSkipsWhenContextCancelled(t *testing.T) {
	primary := &stubModel{name: "primary", failN: 100}
	backup := &stubModel{name: "backup"}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	chain := NewFallbackModel(primary, []model.ToolCallingChatModel{backup}, []string{"backup"})
	if _, err := chain.Generate(ctx, []*schema.Message{{Role: schema.User, Content: "hi"}}); err == nil {
		t.Fatal("ctx 已取消时应返回错误")
	}
	if got := atomic.LoadInt32(&backup.calls); got != 0 {
		t.Fatalf("ctx 取消后不应切换备用模型，实际调用 %d 次", got)
	}
}

// TestFallbackWithoutBackupBehavesLikeSingleModel 未配置备用模型时，
// 行为应退化为单模型（失败直接报错）。
func TestFallbackWithoutBackupBehavesLikeSingleModel(t *testing.T) {
	primary := &stubModel{name: "primary", failN: 100}
	chain := NewFallbackModel(primary, nil, nil)

	if chain.HasFallback() {
		t.Fatal("未配置备用模型时 HasFallback 应为 false")
	}
	if _, err := chain.Generate(context.Background(), []*schema.Message{{Role: schema.User, Content: "hi"}}); err == nil {
		t.Fatal("恒失败时应返回错误")
	}
}
