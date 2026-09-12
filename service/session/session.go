package session

import (
	"context"
	"deeptalk/common/aihelper"
	"deeptalk/common/code"
	"deeptalk/common/metrics"
	"deeptalk/common/resilience"
	"deeptalk/dao/message"
	"deeptalk/dao/session"
	"deeptalk/model"
	"encoding/json"
	"errors"
	"log"
	"net/http"

	"github.com/google/uuid"
)

// mapAIError 把 AI 层错误映射成业务码：
//   - 每日配额用尽 → 4003
//   - 熔断打开（上游连续失败，快速拒绝）→ 5004
//   - 其它 → 5003 模型运行失败
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
	if modelType == "" {
		return aihelper.DefaultModelType, true
	}
	if !aihelper.IsValidModelType(modelType) {
		return "", false
	}
	return modelType, true
}

// resolveBoundModelType 读取会话绑定的模型类型
// 会话的模型在创建时确定，之后不允许修改；同时校验会话归属，避免越权访问他人会话
func resolveBoundModelType(userName, sessionID string) (string, code.Code) {
	s, err := session.GetSessionByID(sessionID)
	if err != nil {
		log.Printf("[session] GetSessionByID(%s) failed: %v", sessionID, err)
		return "", code.CodeSessionNotExist
	}
	if s.UserName != userName {
		log.Printf("[session] user %s tried to access session %s owned by %s", userName, sessionID, s.UserName)
		return "", code.CodeSessionNotExist
	}
	// 历史数据可能没有记录模型类型，按默认模型兜底
	if s.ModelType == "" || !aihelper.IsValidModelType(s.ModelType) {
		log.Printf("[session] session %s has no valid model_type=%q, fallback to %s", sessionID, s.ModelType, aihelper.DefaultModelType)
		return aihelper.DefaultModelType, code.CodeSuccess
	}
	return s.ModelType, code.CodeSuccess
}

// getHelper 获取（或创建）会话对应的 AIHelper，模型类型始终以会话绑定值为准
func getHelper(userName, sessionID, boundModelType string) (*aihelper.AIHelper, code.Code) {
	manager := aihelper.GetGlobalManager()
	config := map[string]interface{}{
		"username": userName, // 用于 RAG 模型获取用户文档
	}
	helper, err := manager.GetOrCreateAIHelper(userName, sessionID, boundModelType, config)
	if err != nil {
		log.Printf("[session] GetOrCreateAIHelper(user=%s session=%s modelType=%s) failed: %v", userName, sessionID, boundModelType, err)
		return nil, code.AIModelFail
	}
	return helper, code.CodeSuccess
}

func GetUserSessionsByUserName(ctx context.Context, userName string) ([]model.SessionInfo, error) {
	//从数据库获取用户的所有会话信息
	Sessions, err := session.GetSessionsByUserName(userName)
	if err != nil {
		log.Println("GetUserSessionsByUserName GetSessionsByUserName error:", err)
		return nil, err
	}

	var SessionInfos []model.SessionInfo

	for _, s := range Sessions {
		modelType := s.ModelType
		if modelType == "" || !aihelper.IsValidModelType(modelType) {
			modelType = aihelper.DefaultModelType
		}
		SessionInfos = append(SessionInfos, model.SessionInfo{
			SessionID: s.ID,
			Title:     s.Title, // 使用数据库中保存的标题（用户的第一个问题）
			ModelType: modelType,
		})
	}

	return SessionInfos, nil
}

func CreateSessionAndSendMessage(ctx context.Context, userName string, userQuestion string, modelType string) (string, string, code.Code) {
	modelType, ok := normalizeModelType(modelType)
	if !ok {
		log.Printf("[session] invalid modelType=%q from user=%s", modelType, userName)
		return "", "", code.CodeInvalidParams
	}

	//1：创建一个新的会话（模型类型在此刻绑定）
	newSession := &model.Session{
		ID:        uuid.New().String(),
		UserName:  userName,
		Title:     userQuestion, // 可以根据需求设置标题，这边暂时用用户第一次的问题作为标题
		ModelType: modelType,
	}
	createdSession, err := session.CreateSession(newSession)
	if err != nil {
		log.Println("CreateSessionAndSendMessage CreateSession error:", err)
		return "", "", code.CodeServerBusy
	}

	//2：获取AIHelper并通过其管理消息
	helper, code_ := getHelper(userName, createdSession.ID, modelType)
	if code_ != code.CodeSuccess {
		return "", "", code_
	}

	//3：生成AI回复
	aiResponse, err_ := helper.GenerateResponse(ctx, userName, userQuestion)
	if err_ != nil {
		log.Println("CreateSessionAndSendMessage GenerateResponse error:", err_)
		return "", "", mapAIError(err_)
	}

	return createdSession.ID, aiResponse.Content, code.CodeSuccess
}

func CreateStreamSessionOnly(ctx context.Context, userName string, userQuestion string, modelType string) (string, code.Code) {
	modelType, ok := normalizeModelType(modelType)
	if !ok {
		log.Printf("[session] invalid modelType=%q from user=%s", modelType, userName)
		return "", code.CodeInvalidParams
	}

	newSession := &model.Session{
		ID:        uuid.New().String(),
		UserName:  userName,
		Title:     userQuestion,
		ModelType: modelType,
	}
	createdSession, err := session.CreateSession(newSession)
	if err != nil {
		log.Println("CreateStreamSessionOnly CreateSession error:", err)
		return "", code.CodeServerBusy
	}
	return createdSession.ID, code.CodeSuccess
}

func StreamMessageToExistingSession(ctx context.Context, userName string, sessionID string, userQuestion string, modelType string, writer http.ResponseWriter) code.Code {
	// 确保 writer 支持 Flush
	flusher, ok := writer.(http.Flusher)
	if !ok {
		log.Println("StreamMessageToExistingSession: streaming unsupported")
		return code.CodeServerBusy
	}

	// 会话绑定的模型优先：请求里携带的 modelType 只在新建会话时生效
	boundModelType, code_ := resolveBoundModelType(userName, sessionID)
	if code_ != code.CodeSuccess {
		return code_
	}
	if modelType != "" && modelType != boundModelType {
		log.Printf("[session] ignore modelType=%s in request, session=%s is bound to %s", modelType, sessionID, boundModelType)
	}

	helper, code_ := getHelper(userName, sessionID, boundModelType)
	if code_ != code.CodeSuccess {
		return code_
	}

	// 客户端断开时取消上游生成，避免继续消耗 token
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	writeFailed := false
	cb := func(msg string) {
		if writeFailed {
			return
		}
		// SSE 载荷统一使用 JSON，避免正文里的换行/特殊字符破坏事件帧
		payload, err := json.Marshal(map[string]string{"content": msg})
		if err != nil {
			log.Println("[SSE] marshal chunk error:", err)
			return
		}
		if _, err := writer.Write([]byte("data: " + string(payload) + "\n\n")); err != nil {
			log.Println("[SSE] Write error:", err)
			writeFailed = true
			cancel() // 客户端已断开，停止继续生成
			return
		}
		flusher.Flush() //  每次必须 flush
	}

	_, err_ := helper.StreamResponse(streamCtx, userName, cb, userQuestion)
	if err_ != nil {
		log.Println("StreamMessageToExistingSession StreamResponse error:", err_)
		if writeFailed {
			// 客户端已断开，无法再回传错误
			return code.CodeSuccess
		}
		return mapAIError(err_)
	}

	if writeFailed {
		log.Printf("[SSE] client gone, stop streaming session=%s", sessionID)
		return code.CodeSuccess
	}

	_, err := writer.Write([]byte("data: [DONE]\n\n"))
	if err != nil {
		log.Println("StreamMessageToExistingSession write DONE error:", err)
		return code.AIModelFail
	}
	flusher.Flush()

	return code.CodeSuccess
}

// WriteSSEError 以统一的 JSON 事件格式向前端回传错误
func WriteSSEError(writer http.ResponseWriter, msg string) {
	payload, err := json.Marshal(map[string]string{"error": msg})
	if err != nil {
		return
	}
	_, _ = writer.Write([]byte("data: " + string(payload) + "\n\n"))
	if flusher, ok := writer.(http.Flusher); ok {
		flusher.Flush()
	}
}

func ChatSend(ctx context.Context, userName string, sessionID string, userQuestion string, modelType string) (string, code.Code) {
	//1：会话绑定的模型优先
	boundModelType, code_ := resolveBoundModelType(userName, sessionID)
	if code_ != code.CodeSuccess {
		return "", code_
	}
	if modelType != "" && modelType != boundModelType {
		log.Printf("[session] ignore modelType=%s in request, session=%s is bound to %s", modelType, sessionID, boundModelType)
	}

	helper, code_ := getHelper(userName, sessionID, boundModelType)
	if code_ != code.CodeSuccess {
		return "", code_
	}

	//2：生成AI回复
	aiResponse, err_ := helper.GenerateResponse(ctx, userName, userQuestion)
	if err_ != nil {
		log.Println("ChatSend GenerateResponse error:", err_)
		return "", mapAIError(err_)
	}

	return aiResponse.Content, code.CodeSuccess
}

func GetChatHistory(ctx context.Context, userName string, sessionID string) ([]model.History, code.Code) {
	// 校验会话归属
	boundModelType, code_ := resolveBoundModelType(userName, sessionID)
	if code_ != code.CodeSuccess {
		return nil, code_
	}

	manager := aihelper.GetGlobalManager()
	helper, exists := manager.GetAIHelper(userName, sessionID)
	if !exists {
		// 内存里没有（服务刚重启/已被回收）时从数据库加载，直接以数据库为准
		msgs, err := message.GetMessagesBySessionID(sessionID)
		if err != nil {
			log.Printf("[session] GetMessagesBySessionID(%s) failed: %v", sessionID, err)
			return nil, code.CodeServerBusy
		}
		if len(msgs) == 0 {
			// 会话为空时也把 helper 建出来，便于后续恢复会话绑定
			if _, code_ := getHelper(userName, sessionID, boundModelType); code_ != code.CodeSuccess {
				return nil, code_
			}
		}
		dbMsgs := make([]*model.Message, 0, len(msgs))
		for i := range msgs {
			dbMsgs = append(dbMsgs, &msgs[i])
		}
		return toHistory(dbMsgs), code.CodeSuccess
	}

	return toHistory(helper.GetMessages()), code.CodeSuccess
}

func toHistory(messages []*model.Message) []model.History {
	history := make([]model.History, 0, len(messages))
	for _, msg := range messages {
		history = append(history, model.History{
			IsUser:  msg.IsUser, // 直接使用落库的角色，不再靠下标奇偶推断
			Content: msg.Content,
		})
	}
	return history
}

func ChatStreamSend(ctx context.Context, userName string, sessionID string, userQuestion string, modelType string, writer http.ResponseWriter) code.Code {
	return StreamMessageToExistingSession(ctx, userName, sessionID, userQuestion, modelType, writer)
}
