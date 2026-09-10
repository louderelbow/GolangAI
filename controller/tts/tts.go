package tts

import (
	"deeptalk/common/code"
	"deeptalk/common/tts"
	"deeptalk/controller"
	"fmt"
	"log"
	"net/http"

	"github.com/gin-gonic/gin"
)

type (
	TTSRequest struct {
		// 单次合成的文本上限，过长会拼接多次上游请求（也限制滥用成本）
		Text string `json:"text" binding:"required,max=1000"`
	}

	PlayTTSResponse struct {
		controller.Response
	}
)

// PlayTTS 直接返回语音流，一次请求即可播放，相同文本走缓存
func PlayTTS(c *gin.Context) {
	req := new(TTSRequest)
	res := new(PlayTTSResponse)
	if err := c.ShouldBindJSON(req); err != nil {
		log.Printf("[TTS] invalid params: %v", err)
		c.JSON(http.StatusOK, res.CodeOf(code.CodeInvalidParams))
		return
	}

	ttsSvc := tts.NewTTSService()
	audioBytes, err := ttsSvc.GetOrCreateTTS(c.Request.Context(), req.Text)
	if err != nil {
		log.Printf("[TTS] GetOrCreateTTS failed: %v", err)
		c.JSON(http.StatusOK, res.CodeOf(code.CodeServerBusy))
		return
	}

	c.Header("Content-Type", "audio/mp3")
	c.Header("Content-Length", fmt.Sprintf("%d", len(audioBytes)))
	c.Writer.Write(audioBytes)
}
