package session

import (
	"context"
	"log"

	"deeptalk/common/code"
)

func ChatSend(ctx context.Context, userName, sessionID, question, requestedModelType string) (string, code.Code) {
	modelType, resultCode := resolveBoundModelType(userName, sessionID)
	if resultCode != code.CodeSuccess {
		return "", resultCode
	}
	logIgnoredModelType(requestedModelType, modelType, sessionID)

	helper, resultCode := getHelper(userName, sessionID, modelType)
	if resultCode != code.CodeSuccess {
		return "", resultCode
	}
	response, err := helper.GenerateResponse(ctx, userName, question)
	if err != nil {
		log.Printf("[session] generate response: %v", err)
		return "", mapAIError(err)
	}
	return response.Content, code.CodeSuccess
}

func logIgnoredModelType(requested, bound, sessionID string) {
	if requested != "" && requested != bound {
		log.Printf("[session] ignore modelType=%s, session=%s is bound to %s", requested, sessionID, bound)
	}
}
