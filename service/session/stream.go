package session

import (
	"context"
	"log"

	"deeptalk/common/code"
	"deeptalk/internal/agent/askuser"
	"deeptalk/internal/infra/metrics"
)

// StreamMessageToExistingSession 只编排业务流，具体 SSE/HTTP 编码由 Controller 负责。
//
// 返回的 *askuser.Request 非 nil 表示本轮 Agent 反问用户：这时没有任何内容
// 流过 onChunk，Controller 应改发一个 clarify 事件而不是把它当正文推送。
//
// warnings 是本轮的工具告警（工具失败 / 被熔断）。它不是失败，本轮照常
// 有回答；Controller 应把它作为提示发给前端，让用户知道回答可能不完整。
func StreamMessageToExistingSession(
	ctx context.Context,
	userName, sessionID, question, requestedModelType string,
	cl *ClarifyContext,
	onChunk func(string) error,
) (*askuser.Request, []string, code.Code) {
	modelType, resultCode := resolveBoundModelType(userName, sessionID)
	if resultCode != code.CodeSuccess {
		return nil, nil, resultCode
	}
	logIgnoredModelType(requestedModelType, modelType, sessionID)

	helper, resultCode := getHelper(userName, sessionID, modelType)
	if resultCode != code.CodeSuccess {
		return nil, nil, resultCode
	}

	// 与同步路径同理：澄清的"答"在这一轮，"问"在上一轮，只能直接记指标。
	if cl != nil {
		metrics.CountAgentClarify("answered")
	}

	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	writeFailed := false
	_, err := helper.StreamResponse(streamCtx, userName, func(chunk string) {
		if writeFailed || onChunk == nil {
			return
		}
		if callbackErr := onChunk(chunk); callbackErr != nil {
			writeFailed = true
			cancel()
		}
	}, augmentQuestion(question, cl))

	// 两个附加信息都要取走，否则会漏到下一轮（同 ChatSend）
	warnings := helper.TakeWarnings()

	if writeFailed {
		// 客户端断开了：不算失败，也不该再往回写任何东西
		return nil, warnings, code.CodeSuccess
	}

	// 澄清请求优先于错误（同 ChatSend）
	if req := helper.TakeClarify(); req != nil {
		return req, warnings, code.CodeNeedClarify
	}
	if err != nil {
		log.Printf("[session] stream response: %v", err)
		return nil, warnings, mapAIError(err)
	}
	return nil, warnings, code.CodeSuccess
}
