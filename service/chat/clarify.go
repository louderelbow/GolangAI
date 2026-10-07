package chat

import (
	"context"
	"log"
	"time"

	"deeptalk/internal/agent/askuser"
	"deeptalk/internal/infra/metrics"
)

// finishClarify 检查本轮是否以"反问用户"结束。
//
// 返回非 nil 表示本轮没有答案，而是抛出了一个澄清提问。要做三件事：
//
//  1. 把它挂到 helper 上，让上层能区分"这是提问"和"这是回答"
//  2. 把提问本身作为**助手发言**落库。澄清那一轮助手确实说了一句话
//     （就是这句提问），存下来历史才是严格的 user/assistant 交替；
//     否则下一轮带上补充信息后会变成两条连续的 user 消息，
//     部分上游会因此直接报错。
//  3. 记账：这一轮模型被真实调用过（它得先想清楚要不要问），
//     所以算一次请求，但不产生 token 消耗。
func (a *AIHelper) finishClarify(ctx context.Context, userName, modelType, modelName string, start time.Time) *askuser.Request {
	collector, ok := askuser.FromContext(ctx)
	if !ok {
		// 没有收集器 = 本轮没开澄清能力，行为与从前完全一致
		return nil
	}
	req, has := collector.Take()
	if !has {
		return nil
	}

	a.setClarify(req)
	a.AddMessage(req.AsText(), userName, false, true)

	metrics.RecordAIRequest(metrics.AIRequest{
		Model:     modelName,
		ModelType: modelType,
		User:      userName,
		Status:    "clarify",
		Source:    "agent",
		Latency:   time.Since(start),
	})
	log.Printf("[AIHelper] ask_user session=%s question=%.40s options=%d",
		a.SessionID, req.Question, len(req.Options))
	return &req
}
