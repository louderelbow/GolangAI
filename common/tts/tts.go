package tts

import (
	"bytes"
	"context"
	"crypto/md5"
	"deeptalk/config"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sync"
)

var audioCache sync.Map

// ------------------ TTS Service ------------------

type TTSService struct{}

func NewTTSService() *TTSService {
	return &TTSService{}
}

// GetOrCreateTTS 根据文本获取语音，相同文本走缓存，只调一次百度API
func (s *TTSService) GetOrCreateTTS(ctx context.Context, text string) ([]byte, error) {
	hash := fmt.Sprintf("%x", md5.Sum([]byte(text)))
	if cached, ok := audioCache.Load(hash); ok {
		return cached.([]byte), nil
	}

	audioBytes, err := s.callBaiduAPI(ctx, text)
	if err != nil {
		return nil, err
	}

	audioCache.Store(hash, audioBytes)
	return audioBytes, nil
}

// callBaiduAPI 调用百度短文本TTS同步接口
func (s *TTSService) callBaiduAPI(ctx context.Context, text string) ([]byte, error) {
	accessToken := s.getAccessToken()
	if accessToken == "" {
		return nil, fmt.Errorf("failed to get access token")
	}

	syncURL := "https://tsn.baidu.com/text2audio"
	formData := url.Values{}
	formData.Set("tex", text)
	formData.Set("tok", accessToken)
	formData.Set("cuid", "deeptalk")
	formData.Set("ctp", "1")
	formData.Set("lan", "zh")
	formData.Set("spd", "5")
	formData.Set("pit", "5")
	formData.Set("vol", "5")
	formData.Set("per", "4")
	formData.Set("aue", "3")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, syncURL, bytes.NewReader([]byte(formData.Encode())))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	audioBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.Header.Get("Content-Type") != "audio/mp3" {
		log.Println("[TTS] error:", string(audioBytes))
		return nil, fmt.Errorf("tts failed: %s", string(audioBytes))
	}

	log.Println("[TTS] success, size:", len(audioBytes))
	return audioBytes, nil
}

// getAccessToken 获取百度API access_token
func (s *TTSService) getAccessToken() string {
	conf := config.GetConfig()

	apiURL := "https://aip.baidubce.com/oauth/2.0/token"
	postData := fmt.Sprintf(
		"grant_type=client_credentials&client_id=%s&client_secret=%s",
		conf.VoiceServiceConfig.VoiceServiceApiKey,
		conf.VoiceServiceConfig.VoiceServiceSecretKey,
	)

	resp, err := http.Post(apiURL, "application/x-www-form-urlencoded", bytes.NewReader([]byte(postData)))
	if err != nil {
		log.Println("get token error:", err)
		return ""
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Println("read token error:", err)
		return ""
	}

	var tokenResp struct {
		AccessToken string `json:"access_token"`
	}

	if err := json.Unmarshal(body, &tokenResp); err != nil {
		log.Println("unmarshal token error:", err)
		return ""
	}

	return tokenResp.AccessToken
}
