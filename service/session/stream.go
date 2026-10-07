package session

import (
	"context"
	"log"

	"deeptalk/common/code"
	"deeptalk/internal/agent/askuser"
)

// StreamMessageToExistingSession 只编排业务流，具体 SSE/HTTP 编码由 Controller 负责。
//
// 返回的 *askuser.Request 非 nil 表示本轮 Agent 反问用户：这时没有任何内容
// 流过 onChunk，Controller 应改发一个 clarify 事件而不是把它当正文推送。
func StreamMessageToExistingSession(
	ctx context.Context,
	userName, sessionID, question, requestedModelType string,
	cl *ClarifyContext,
	onChunk func(string) error,
) (*askuser.Request, code.Code) {
	modelType, resultCode := resolveBoundModelType(userName, sessionID)
	if resultCode != code.CodeSuccess {
		return nil, resultCode
	}
	logIgnoredModelType(requestedModelType, modelType, sessionID)

	helper, resultCode := getHelper(userName, sessionID, modelType)
	if resultCode != code.CodeSuccess {
		return nil, resultCode
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

	if writeFailed {
		// 客户端断开了：不算失败，也不该再往回写任何东西
		return nil, code.CodeSuccess
	}

	// 澄清请求优先于错误（同 ChatSend）
	if req := helper.TakeClarify(); req != nil {
		return req, code.CodeNeedClarify
	}
	if err != nil {
		log.Printf("[session] stream response: %v", err)
		return nil, mapAIError(err)
	}
	return nil, code.CodeSuccess
}
