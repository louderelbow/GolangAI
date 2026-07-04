package tts

import (
	"deeptalk/common/tts"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
)

type (
	TTSRequest struct {
		Text string `json:"text,omitempty"`
	}
)
 
// PlayTTS 直接返回语音流，一次请求即可播放，相同文本走缓存
func PlayTTS(c *gin.Context) {
	req := new(TTSRequest)
	if err := c.ShouldBindJSON(req); err != nil || req.Text == "" {
		c.JSON(http.StatusOK, gin.H{"error": "invalid params"})
		return
	}

	ttsSvc := tts.NewTTSService()
	audioBytes, err := ttsSvc.GetOrCreateTTS(c, req.Text)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"error": "tts failed"})
		return
	}

	c.Header("Content-Type", "audio/mp3")
	c.Header("Content-Length", fmt.Sprintf("%d", len(audioBytes)))
	c.Writer.Write(audioBytes)
}
