package session

import (
	"encoding/json"
	"fmt"
	"net/http"

	"deeptalk/common/code"
	service "deeptalk/service/session"

	"github.com/gin-gonic/gin"
)

func CreateStreamSessionAndSendMessage(c *gin.Context) {
	req := new(CreateSessionAndSendMessageRequest)
	if err := c.ShouldBindJSON(req); err != nil {
		c.JSON(http.StatusOK, gin.H{"error": "Invalid parameters"})
		return
	}

	prepareSSE(c)
	sessionID, modelType, resultCode := service.CreateStreamSessionOnly(
		c.Request.Context(), c.GetString("userName"), req.UserQuestion, req.ModelType,
	)
	if resultCode != code.CodeSuccess {
		writeSSEError(c, resultCode.Msg())
		return
	}
	if err := writeSSEJSON(c, map[string]string{"sessionId": sessionID, "modelType": modelType}); err != nil {
		return
	}

	resultCode = service.StreamMessageToExistingSession(
		c.Request.Context(), c.GetString("userName"), sessionID, req.UserQuestion, req.ModelType,
		func(chunk string) error { return writeSSEJSON(c, map[string]string{"content": chunk}) },
	)
	finishSSE(c, resultCode)
}

func ChatStreamSend(c *gin.Context) {
	req := new(ChatSendRequest)
	if err := c.ShouldBindJSON(req); err != nil {
		c.JSON(http.StatusOK, gin.H{"error": "Invalid parameters"})
		return
	}

	prepareSSE(c)
	resultCode := service.StreamMessageToExistingSession(
		c.Request.Context(), c.GetString("userName"), req.SessionID, req.UserQuestion, req.ModelType,
		func(chunk string) error { return writeSSEJSON(c, map[string]string{"content": chunk}) },
	)
	finishSSE(c, resultCode)
}

func prepareSSE(c *gin.Context) {
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("Access-Control-Allow-Origin", "*")
	c.Header("X-Accel-Buffering", "no")
}

func writeSSEJSON(c *gin.Context, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err = fmt.Fprintf(c.Writer, "data: %s\n\n", data); err != nil {
		return err
	}
	c.Writer.Flush()
	return nil
}

func writeSSEError(c *gin.Context, message string) {
	_ = writeSSEJSON(c, map[string]string{"error": message})
}

func finishSSE(c *gin.Context, resultCode code.Code) {
	if resultCode != code.CodeSuccess {
		writeSSEError(c, resultCode.Msg())
		return
	}
	_, _ = c.Writer.WriteString("data: [DONE]\n\n")
	c.Writer.Flush()
}
