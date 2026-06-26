package image

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"io"
	"net/http"
	"os"

	"deeptalk/config"
)

// ======================== 阿里云 DashScope 视觉识别 ========================
// 使用 config.toml 中 ragModelConfig 的 baseUrl + apiKey
// 调用 qwen-vl 多模态模型进行图像理解，替代 ONNX MobileNetV2 本地推理

type ImageRecognizer struct {
	apiKey  string
	baseURL string
	model   string
}

func NewImageRecognizer(modelPath, labelPath string, inputH, inputW int) (*ImageRecognizer, error) {
	conf := config.GetConfig()
	apiKey := conf.RagModelConfig.RagApiKey
	if apiKey == "" {
		apiKey = os.Getenv("ALIYUN_API_KEY")
	}
	if apiKey == "" {
		apiKey = os.Getenv("DEEPSEEK_API_KEY")
	}
	if apiKey == "" {
		apiKey = os.Getenv("OPENAI_API_KEY")
	}

	baseURL := conf.RagModelConfig.RagBaseUrl
	if baseURL == "" {
		baseURL = "https://dashscope.aliyuncs.com/compatible-mode/v1"
	}

	return &ImageRecognizer{
		apiKey:  apiKey,
		baseURL: baseURL,
		model:   "qwen-vl-plus",
	}, nil
}

func (r *ImageRecognizer) Close() {}

// PredictFromFile 从文件路径识别图像
func (r *ImageRecognizer) PredictFromFile(imagePath string) (string, error) {
	data, err := os.ReadFile(imagePath)
	if err != nil {
		return "", fmt.Errorf("read image file: %w", err)
	}
	return r.callVisionAPI(data)
}

// PredictFromBuffer 从字节缓冲识别图像
func (r *ImageRecognizer) PredictFromBuffer(buf []byte) (string, error) {
	return r.callVisionAPI(buf)
}

// PredictFromImage 从 image.Image 识别（暂不支持，请使用 PredictFromFile 或 PredictFromBuffer）
func (r *ImageRecognizer) PredictFromImage(img image.Image) (string, error) {
	return "", fmt.Errorf("PredictFromImage not implemented, use PredictFromFile or PredictFromBuffer")
}

// callVisionAPI 调用阿里云 DashScope 多模态 API
func (r *ImageRecognizer) callVisionAPI(imageData []byte) (string, error) {
	// Base64 编码图片
	imageBase64 := base64.StdEncoding.EncodeToString(imageData)

	// 构造 OpenAI 兼容格式的 multimodal 请求
	reqBody := map[string]interface{}{
		"model": r.model,
		"messages": []map[string]interface{}{
			{
				"role": "user",
				"content": []map[string]interface{}{
					{
						"type": "image_url",
						"image_url": map[string]string{
							"url": "data:image/jpeg;base64," + imageBase64,
						},
					},
					{
						"type": "text",
						"text": "请识别这张图片的内容，用中文简短描述图片里有什么。如果是物体或动物，给出具体的名称。",
					},
				},
			},
		},
		"max_tokens": 200,
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}

	url := r.baseURL + "/chat/completions"
	req, err := http.NewRequest("POST", url, bytes.NewReader(bodyBytes))
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+r.apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("api call failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("api returned status %d: %s", resp.StatusCode, string(respBody))
	}

	// 解析 OpenAI 格式响应
	var result struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("parse response: %w", err)
	}

	if len(result.Choices) == 0 {
		return "未识别到内容", nil
	}

	return result.Choices[0].Message.Content, nil
}
