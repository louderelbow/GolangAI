package inference

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"deeptalk/internal/infra/config"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// ==================== 测试替身 ====================

// fakeModel 可控的假模型：可让前 N 次失败，可注入延迟。
type fakeModel struct {
	failN int32
	delay time.Duration

	calls int32
}

func (m *fakeModel) WithTools([]*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return m, nil
}

func (m *fakeModel) Generate(ctx context.Context, in []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	n := atomic.AddInt32(&m.calls, 1)
	if m.delay > 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(m.delay):
		}
	}
	if n <= atomic.LoadInt32(&m.failN) {
		return nil, errors.New("upstream boom")
	}
	return &schema.Message{Role: schema.Assistant, Content: "ok"}, nil
}

func (m *fakeModel) Stream(ctx context.Context, in []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	msg, err := m.Generate(ctx, in, opts...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{msg}), nil
}

func testScheduler(t *testing.T, pool config.InferencePoolConfig, breaker config.InferenceBreakerConfig) *Scheduler {
	t.Helper()
	return NewScheduler(config.InferenceConfig{
		Enabled:        true,
		QueueTimeoutMs: 2000,
		MaxQueueDepth:  50,
		Pools:          map[string]config.InferencePoolConfig{"m1": pool},
		Breaker:        breaker,
	})
}

// drainStream 把流转完，模拟 core 的收流行为。
func drainStream(t *testing.T, sr *schema.StreamReader[*schema.Message]) string {
	t.Helper()
	var out string
	for {
		msg, err := sr.Recv()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("收流失败: %v", err)
		}
		out += msg.Content
	}
}

// ==================== 熔断 ====================

// TestBreakerOpensAfterConsecutiveFailures 验收"熔断生效"：
// 连续失败达到阈值后打开，后续请求**不再调用上游**、快速失败。
func TestBreakerOpensAfterConsecutiveFailures(t *testing.T) {
	const threshold = 5

	s := testScheduler(t,
		config.InferencePoolConfig{MaxConcurrent: 4, Weight: 1},
		config.InferenceBreakerConfig{FailureThreshold: threshold, OpenDurationMs: 60000, HalfOpenProbes: 3})

	inner := &fakeModel{failN: 1000} // 恒失败
	wrapped := s.Wrap("m1", inner)
	ctx := context.Background()

	for i := 0; i < threshold; i++ {
		if _, err := wrapped.Generate(ctx, nil); err == nil {
			t.Fatalf("第 %d 次调用应失败", i+1)
		}
	}
	if got := s.BreakerState("m1"); got != StateOpen {
		t.Fatalf("连续失败 %d 次后应打开熔断，实际状态 %s", threshold, got)
	}

	callsBefore := atomic.LoadInt32(&inner.calls)
	_, err := wrapped.Generate(ctx, nil)
	if !errors.Is(err, ErrBreakerOpen) {
		t.Fatalf("熔断打开后应返回 ErrBreakerOpen，实际: %v", err)
	}
	if got := atomic.LoadInt32(&inner.calls); got != callsBefore {
		t.Fatalf("熔断打开后不应再打到上游：before=%d after=%d", callsBefore, got)
	}
}

// TestBreakerHalfOpenRecovers 验收"半开恢复"：
// 打开时长过后允许探测，探测成功则回到 closed。
func TestBreakerHalfOpenRecovers(t *testing.T) {
	// HalfOpenProbes=1：一次成功的探测就足以确认上游恢复、关闭熔断
	s := testScheduler(t,
		config.InferencePoolConfig{MaxConcurrent: 2, Weight: 1},
		config.InferenceBreakerConfig{FailureThreshold: 2, OpenDurationMs: 80, HalfOpenProbes: 1})

	inner := &fakeModel{failN: 2} // 前两次失败，之后成功
	wrapped := s.Wrap("m1", inner)
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		_, _ = wrapped.Generate(ctx, nil)
	}
	if got := s.BreakerState("m1"); got != StateOpen {
		t.Fatalf("应已打开，实际 %s", got)
	}

	// open 期内：快速失败
	if _, err := wrapped.Generate(ctx, nil); !errors.Is(err, ErrBreakerOpen) {
		t.Fatalf("open 期内应快速失败，实际: %v", err)
	}

	time.Sleep(120 * time.Millisecond)

	// 半开探测：上游已恢复，应成功并关闭熔断
	if _, err := wrapped.Generate(ctx, nil); err != nil {
		t.Fatalf("半开探测应放行并成功，实际: %v", err)
	}
	if got := s.BreakerState("m1"); got != StateClosed {
		t.Fatalf("探测成功后应关闭，实际 %s", got)
	}
}

// ==================== 调度器包装 ====================

func TestSchedulerDisabledIsPassThrough(t *testing.T) {
	s := NewScheduler(config.InferenceConfig{Enabled: false})
	inner := &fakeModel{}
	wrapped := s.Wrap("m1", inner)
	if wrapped != model.ToolCallingChatModel(inner) {
		t.Fatal("关闭调度时应原样返回模型")
	}
	if s.Enabled() {
		t.Fatal("Enabled 应为 false")
	}
	// 关闭状态下 Drain/Close 不应 panic
	s.Close()
	if !s.Drain(time.Millisecond) {
		t.Fatal("关闭调度时 Drain 应返回 true")
	}
}

// TestSchedulerHoldsSlotForWholeStream 流式请求必须占用槽位到读完为止，
// 否则并发上限对流式请求形同虚设（而流式是本项目主要形态）。
func TestSchedulerHoldsSlotForWholeStream(t *testing.T) {
	s := testScheduler(t,
		config.InferencePoolConfig{MaxConcurrent: 1, Weight: 1},
		config.InferenceBreakerConfig{FailureThreshold: 100, OpenDurationMs: 1000, HalfOpenProbes: 1})

	inner := &fakeModel{}
	wrapped := s.Wrap("m1", inner)

	sr, err := wrapped.Stream(context.Background(), nil)
	if err != nil {
		t.Fatalf("建流失败: %v", err)
	}

	// 流已建立但还没读完：槽位必须仍被占着
	if ok := waitFor(t, 500*time.Millisecond, func() bool {
		inflight, _ := s.Stats("m1")
		return inflight == 1
	}); !ok {
		inflight, queued := s.Stats("m1")
		t.Fatalf("流式期间应占着槽位：inflight=%d queued=%d", inflight, queued)
	}

	if got := drainStream(t, sr); got != "ok" {
		t.Fatalf("流内容不对: %q", got)
	}

	// 读完后槽位必须归还
	if ok := waitFor(t, time.Second, func() bool {
		inflight, queued := s.Stats("m1")
		return inflight == 0 && queued == 0
	}); !ok {
		inflight, queued := s.Stats("m1")
		t.Fatalf("读完流后应归还槽位：inflight=%d queued=%d", inflight, queued)
	}
}

// TestSchedulerGracefulShutdown 验收"优雅关闭"：
// 在途请求正常完成，新请求被拒绝。
func TestSchedulerGracefulShutdown(t *testing.T) {
	s := testScheduler(t,
		config.InferencePoolConfig{MaxConcurrent: 2, Weight: 1},
		config.InferenceBreakerConfig{FailureThreshold: 100, OpenDurationMs: 1000, HalfOpenProbes: 1})

	inner := &fakeModel{delay: 60 * time.Millisecond}
	wrapped := s.Wrap("m1", inner)

	var wg sync.WaitGroup
	wg.Add(1)
	var inflightErr error
	go func() {
		defer wg.Done()
		_, inflightErr = wrapped.Generate(context.Background(), nil)
	}()

	// 等在途请求真的占住槽位
	if !waitFor(t, time.Second, func() bool { i, _ := s.Stats("m1"); return i == 1 }) {
		t.Fatal("在途请求未占住槽位")
	}

	s.Close()

	// 新请求立刻被拒
	if _, err := wrapped.Generate(context.Background(), nil); !errors.Is(err, ErrShuttingDown) {
		t.Fatalf("关闭后新请求应被拒绝，实际: %v", err)
	}

	wg.Wait()
	if inflightErr != nil {
		t.Fatalf("在途请求应正常完成，实际: %v", inflightErr)
	}
	if !s.Drain(2 * time.Second) {
		t.Fatal("在途请求结束后 Drain 应返回 true")
	}
}

// ==================== 负载均衡 ====================

func TestLeastConnPicksIdleInstance(t *testing.T) {
	a := &Instance{ID: "a", Weight: 1}
	b := &Instance{ID: "b", Weight: 1}

	bal := NewBalancer("least_conn", []*Instance{a, b})

	first := bal.Pick()
	second := bal.Pick()

	if first == second {
		t.Fatalf("最少连接应把第二个请求分给空闲实例，实际两次都选了 %s", first.ID)
	}
	bal.Release(first)
	third := bal.Pick()
	if third != first {
		t.Fatalf("归还后应重新选到空闲的 %s，实际 %s", first.ID, third.ID)
	}
}

func TestWeightedRoundRobinRespectsWeights(t *testing.T) {
	a := &Instance{ID: "a", Weight: 3}
	b := &Instance{ID: "b", Weight: 1}

	bal := NewBalancer("weighted", []*Instance{a, b})

	counts := map[string]int{}
	for i := 0; i < 40; i++ {
		in := bal.Pick()
		counts[in.ID]++
		bal.Release(in)
	}
	if counts["a"] != 30 || counts["b"] != 10 {
		t.Fatalf("3:1 权重在 40 次里应得到 30/10，实际 %v", counts)
	}
}

// TestSmoothWRRDistributesNonDivisibleWeights 平滑加权轮询的价值体现在
// 权重不能整除的比例上：它把多余的请求均匀摊开，而不是堆成一段连发。
//
// 注意 3:1 这种比例连发 3 次是理论最优（无法更均匀），所以这里用 5:1:1 验证。
func TestSmoothWRRDistributesNonDivisibleWeights(t *testing.T) {
	a := &Instance{ID: "a", Weight: 5}
	b := &Instance{ID: "b", Weight: 1}
	c := &Instance{ID: "c", Weight: 1}

	bal := NewBalancer("weighted", []*Instance{a, b, c})

	const rounds = 70 // 5:1:1 → 50 / 10 / 10
	counts := map[string]int{}
	for i := 0; i < rounds; i++ {
		in := bal.Pick()
		counts[in.ID]++
		bal.Release(in)
	}

	if counts["a"] != 50 || counts["b"] != 10 || counts["c"] != 10 {
		t.Fatalf("5:1:1 在 70 次里应得到 50/10/10，实际 %v", counts)
	}
}

func TestBalancerWithoutInstancesReturnsNil(t *testing.T) {
	if got := NewBalancer("least_conn", nil).Pick(); got != nil {
		t.Fatalf("无实例时应返回 nil，实际 %+v", got)
	}
}
