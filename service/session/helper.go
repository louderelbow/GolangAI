package session

import (
	"log"

	"deeptalk/common/code"
	"deeptalk/service/chat"
)

func getHelper(userName, sessionID, modelType string) (*chat.AIHelper, code.Code) {
	helper, err := chat.GetGlobalManager().GetOrCreateAIHelper(
		userName,
		sessionID,
		modelType,
		map[string]interface{}{"username": userName},
	)
	if err != nil {
		log.Printf("[session] create helper user=%s session=%s modelType=%s: %v", userName, sessionID, modelType, err)
		return nil, code.AIModelFail
	}
	return helper, code.CodeSuccess
}
