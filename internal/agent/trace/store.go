package trace

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"deeptalk/internal/infra/metrics"
	"deeptalk/model"
)

// Store 是轨迹的落库实现，由装配处注入。
//
// 做成可注入的函数而不是直接 import dao，是为了让本包在单元测试里
// 不需要拖一个数据库进来——和 metrics.SetQuotaStore 是同一个考虑。
var Store func(*model.AgentTrace) error

// SetStore 装配落库实现。
func SetStore(f func(*model.AgentTrace) error) { Store = f }

// persistTimeout 落库超时。
//
// 轨迹是观测数据，写入再重要也不该让用户多等——所以超时给得很短，
// 失败只记指标（deeptalk_agent_trace_persist_total{result="fail"}），
// 绝不向上抛错打断对话。这条原则和工具失败回填是一致的：
// **观测设施永远不能成为故障点**。
const persistTimeout = 2 * time.Second

// Persist 落库并导出指标。t 的 Status 应当已经由调用方设好。
//
// 两个出口的顺序是刻意的：**先出指标，再落库**。
// 指标是内存操作不会失败；落库可能因为数据库抖动失败。先做不会失败的那个，
// 至少保证"整体趋势"这一层不丢——而它恰恰是更常用的那一层。
func Persist(ctx context.Context, t Trace) {
	Flush(t)

	if Store == nil {
		return
	}

	steps, err := json.Marshal(t.Steps)
	if err != nil {
		metrics.CountAgentTracePersist(false)
		log.Printf("[trace] 序列化轨迹失败 id=%s: %v", t.ID, err)
		return
	}

	row := &model.AgentTrace{
		ID:         t.ID,
		SessionID:  t.SessionID,
		UserName:   t.User,
		Model:      t.Model,
		ModelType:  t.ModelType,
		Status:     string(t.Status),
		StepCount:  len(t.Steps),
		ToolCalls:  len(t.Of(KindTool)),
		DurationMs: t.Duration().Milliseconds(),
		Steps:      string(steps),
		CreatedAt:  time.Now(),
	}

	// 用一个和请求解耦的 ctx：用户点了停止/断开连接不该让轨迹丢掉，
	// 那正是最需要看轨迹的时候。
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), persistTimeout)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- Store(row) }()

	select {
	case err := <-done:
		if err != nil {
			metrics.CountAgentTracePersist(false)
			log.Printf("[trace] 落库失败 id=%s: %v", t.ID, err)
			return
		}
		metrics.CountAgentTracePersist(true)
	case <-writeCtx.Done():
		metrics.CountAgentTracePersist(false)
		log.Printf("[trace] 落库超时 id=%s", t.ID)
	}
}
