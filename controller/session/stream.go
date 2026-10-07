package session

import (
	"encoding/json"
	"fmt"
	"net/http"

	"deeptalk/common/code"
	"deeptalk/internal/agent/askuser"
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

	clarify, warnings, resultCode := service.StreamMessageToExistingSession(
		c.Request.Context(), c.GetString("userName"), sessionID, req.UserQuestion, req.ModelType, nil,
		func(chunk string) error { return writeSSEJSON(c, map[string]string{"content": chunk}) },
	)
	finishStream(c, clarify, warnings, resultCode)
}

func ChatStreamSend(c *gin.Context) {
	req := new(ChatSendRequest)
	if err := c.ShouldBindJSON(req); err != nil {
		c.JSON(http.StatusOK, gin.H{"error": "Invalid parameters"})
		return
	}

	prepareSSE(c)
	clarify, warnings, resultCode := service.StreamMessageToExistingSession(
		c.Request.Context(), c.GetString("userName"), req.SessionID, req.UserQuestion, req.ModelType,
		clarifyContext(req.ClarifyAnswer),
		func(chunk string) error { return writeSSEJSON(c, map[string]string{"content": chunk}) },
	)
	finishStream(c, clarify, warnings, resultCode)
}

// finishStream 收尾流式响应。
//
// 顺序：工具告警 → 澄清事件 → 结束标记。
// 澄清事件先于结束标记发出：前端收到 clarify 就知道该弹选择框，
// 而不是显示一个空回答。它带 type 字段，与普通内容块区分开——
// 旧前端只会多忽略一个字段，不会因此报错。
//
// warning 事件同理：它不是错误，本轮照常有内容流出，前端只该弹一个轻提示。
func finishStream(c *gin.Context, clarify *askuser.Request, warnings []string, resultCode code.Code) {
	for _, w := range warnings {
		_ = writeSSEJSON(c, map[string]any{"type": "warning", "message": w})
	}

	if clarify != nil {
		_ = writeSSEJSON(c, map[string]any{
			"type":     "clarify",
			"question": clarify.Question,
			"reason":   clarify.Reason,
			"options":  clarifyOptionMaps(clarify),
		})
		_, _ = c.Writer.WriteString("data: [DONE]\n\n")
		c.Writer.Flush()
		return
	}
	finishSSE(c, resultCode)
}

func clarifyOptionMaps(req *askuser.Request) []map[string]string {
	out := make([]map[string]string, 0, len(req.Options))
	for _, o := range req.Options {
		m := map[string]string{"id": o.ID, "label": o.Label}
		if o.Hint != "" {
			m["hint"] = o.Hint
		}
		out = append(out, m)
	}
	return out
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
