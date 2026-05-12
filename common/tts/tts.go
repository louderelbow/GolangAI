package tts

import (
	"bytes"
	"context"
	"deeptalk/config"
	"encoding/base64"
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

// ------------------ Create TTS ------------------

func (s *TTSService) CreateTTS(ctx context.Context, text string) (string, error) {
	accessToken := s.GetAccessToken()
	if accessToken == "" {
		return "", fmt.Errorf("failed to get access token")
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
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	audioBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	if resp.Header.Get("Content-Type") != "audio/mp3" {
		log.Println("[TTS Sync] error:", string(audioBytes))
		return "", fmt.Errorf("tts sync failed: %s", string(audioBytes))
	}

	taskID := fmt.Sprintf("%d", len(audioBytes)) + "local"
	audioCache.Store(taskID, audioBytes)
	log.Println("[TTS Sync] success, size:", len(audioBytes))
	return taskID, nil
}

// ------------------ Access Token ------------------

func (s *TTSService) GetAccessToken() string {
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

// ------------------ Query TTS ------------------

type TTSTaskResult struct {
	SpeechURL string `json:"speech_url,omitempty"`
}

type TTSTask struct {
	TaskID     string         `json:"task_id"`
	TaskStatus string         `json:"task_status"`
	TaskResult *TTSTaskResult `json:"task_result,omitempty"`
}

type TTSQueryResponse struct {
	LogID     string    `json:"log_id"`
	TasksInfo []TTSTask `json:"tasks_info"`
}

func (s *TTSService) QueryTTSFull(ctx context.Context, taskID string) (*TTSQueryResponse, error) {
	if data, ok := audioCache.Load(taskID); ok {
		audioBytes := data.([]byte)
		dataURL := "data:audio/mp3;base64," + base64.StdEncoding.EncodeToString(audioBytes)
		return &TTSQueryResponse{
			LogID: "local",
			TasksInfo: []TTSTask{{
				TaskID:     taskID,
				TaskStatus: "Success",
				TaskResult: &TTSTaskResult{
					SpeechURL: dataURL,
				},
			}},
		}, nil
	}

	return &TTSQueryResponse{
		LogID: "local",
		TasksInfo: []TTSTask{{
			TaskID:     taskID,
			TaskStatus: "Failed",
		}},
	}, nil
}
