// Package session 编排会话、消息与模型调用的业务流程。
package session

import (
	"errors"
	"log"

	"deeptalk/common/code"
	sessiondao "deeptalk/dao/session"
	"deeptalk/internal/infra/metrics"
	"deeptalk/internal/infra/resilience"
	"deeptalk/internal/llm"
)

func mapAIError(err error) code.Code {
	switch {
	case err == nil:
		return code.CodeSuccess
	case errors.Is(err, metrics.ErrQuotaExceeded):
		return code.CodeQuotaExceeded
	case resilience.IsOpen(err):
		return code.CodeAIServiceUnavailable
	default:
		return code.AIModelFail
	}
}

func normalizeModelType(modelType string) (string, bool) {
	normalized, ok := llm.NormalizeModelType(modelType)
	return normalized, ok && llm.IsValidModelType(normalized)
}

func resolveBoundModelType(userName, sessionID string) (string, code.Code) {
	stored, err := sessiondao.GetSessionByID(sessionID)
	if err != nil || stored.UserName != userName {
		if err != nil {
			log.Printf("[session] GetSessionByID(%s) failed: %v", sessionID, err)
		}
		return "", code.CodeSessionNotExist
	}

	normalized, ok := normalizeModelType(stored.ModelType)
	if !ok {
		log.Printf("[session] session %s has invalid model_type=%q, fallback to %s", sessionID, stored.ModelType, llm.DefaultModelType)
		return llm.DefaultModelType, code.CodeSuccess
	}
	return normalized, code.CodeSuccess
}
