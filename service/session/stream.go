package session

import (
	"context"
	"log"

	"deeptalk/common/code"
)

// StreamMessageToExistingSession 只编排业务流，具体 SSE/HTTP 编码由 Controller 负责。
func StreamMessageToExistingSession(
	ctx context.Context,
	userName, sessionID, question, requestedModelType string,
	onChunk func(string) error,
) code.Code {
	modelType, resultCode := resolveBoundModelType(userName, sessionID)
	if resultCode != code.CodeSuccess {
		return resultCode
	}
	logIgnoredModelType(requestedModelType, modelType, sessionID)

	helper, resultCode := getHelper(userName, sessionID, modelType)
	if resultCode != code.CodeSuccess {
		return resultCode
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
	}, question)
	if writeFailed {
		return code.CodeSuccess
	}
	if err != nil {
		log.Printf("[session] stream response: %v", err)
		return mapAIError(err)
	}
	return code.CodeSuccess
}
