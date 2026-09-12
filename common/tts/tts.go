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
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	baiduMaxCharsPerRequest = 50
	// 单次请求最多合成的字符数
	maxTextRunes = 1000
	httpTimeout  = 15 * time.Second

	// 缓存上限：避免音频数据无限占用内存
	maxCacheEntries = 200
	maxCacheBytes   = 32 << 20 // 32MB
)

var httpClient = &http.Client{Timeout: httpTimeout}

// ------------------ 有界音频缓存 ------------------

type audioStore struct {
	mu    sync.Mutex
	items map[string][]byte
	order []string
	bytes int
}

var audioCache = &audioStore{items: make(map[string][]byte)}

func (c *audioStore) get(key string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.items[key]
	return v, ok
}

func (c *audioStore) set(key string, val []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.items[key]; !exists {
		c.order = append(c.order, key)
	}
	c.items[key] = val
	c.bytes += len(val)

	// FIFO 淘汰
	for len(c.order) > maxCacheEntries || c.bytes > maxCacheBytes {
		if len(c.order) == 0 {
			break
		}
		oldest := c.order[0]
		c.order = c.order[1:]
		if v, ok := c.items[oldest]; ok {
			c.bytes -= len(v)
			delete(c.items, oldest)
		}
	}
}

// ------------------ TTS Service ------------------

type TTSService struct{}

func NewTTSService() *TTSService {
	return &TTSService{}
}

// GetOrCreateTTS 根据文本获取语音，相同文本走缓存，只调一次百度API
func (s *TTSService) GetOrCreateTTS(ctx context.Context, text string) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, fmt.Errorf("empty text")
	}
	runes := []rune(text)
	if len(runes) > maxTextRunes {
		runes = runes[:maxTextRunes]
	}
	text = string(runes)

	hash := fmt.Sprintf("%x", md5.Sum([]byte(text)))
	if cached, ok := audioCache.get(hash); ok {
		return cached, nil
	}

	parts := splitText(text, baiduMaxCharsPerRequest)
	audioBytes := make([]byte, 0, 64*1024)
	for _, part := range parts {
		chunk, err := s.callBaiduAPI(ctx, part)
		if err != nil {
			return nil, err
		}
		audioBytes = append(audioBytes, chunk...)
	}

	audioCache.set(hash, audioBytes)
	return audioBytes, nil
}

// splitText 按标点切分文本，尽量保证每片不超过 maxChars 且语义完整
func splitText(text string, maxChars int) []string {
	runes := []rune(text)
	if len(runes) <= maxChars {
		return []string{text}
	}

	parts := make([]string, 0, len(runes)/maxChars+1)
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			parts = append(parts, string(cur))
			cur = cur[:0]
		}
	}

	for _, r := range runes {
		cur = append(cur, r)
		// 遇到句末标点且已经攒够一定长度就切一刀
		if len(cur) >= maxChars || (isSentenceEnd(r) && len(cur) >= maxChars/2) {
			flush()
		}
	}
	flush()

	return parts
}

func isSentenceEnd(r rune) bool {
	switch r {
	case '。', '！', '？', '；', '\n', '.', '!', '?', ';':
		return true
	}
	return false
}

// callBaiduAPI 调用百度短文本TTS同步接口
func (s *TTSService) callBaiduAPI(ctx context.Context, text string) ([]byte, error) {
	accessToken, err := s.getAccessToken(ctx)
	if err != nil {
		return nil, err
	}

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

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://tsn.baidu.com/text2audio", bytes.NewReader([]byte(formData.Encode())))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	audioBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "audio/") {
		log.Printf("[TTS] upstream error, status=%d body=%s", resp.StatusCode, string(audioBytes))
		return nil, fmt.Errorf("tts failed: status=%d", resp.StatusCode)
	}

	log.Printf("[TTS] chunk ok, runes=%d size=%d", utf8.RuneCountInString(text), len(audioBytes))
	return audioBytes, nil
}

var (
	tokenMu     sync.Mutex
	cachedToken string
	tokenExpire time.Time
	tokenTTL    = 25 * 24 * time.Hour
)

// getAccessToken 获取百度API access_token
func (s *TTSService) getAccessToken(ctx context.Context) (string, error) {
	tokenMu.Lock()
	defer tokenMu.Unlock()

	if cachedToken != "" && time.Now().Before(tokenExpire) {
		return cachedToken, nil
	}

	conf := config.GetConfig()
	postData := url.Values{}
	postData.Set("grant_type", "client_credentials")
	postData.Set("client_id", conf.VoiceServiceConfig.VoiceServiceApiKey)
	postData.Set("client_secret", conf.VoiceServiceConfig.VoiceServiceSecretKey)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://aip.baidubce.com/oauth/2.0/token",
		bytes.NewReader([]byte(postData.Encode())))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	var tokenResp struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		ErrorDes    string `json:"error_description"`
	}
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return "", fmt.Errorf("unmarshal token: %w", err)
	}
	if tokenResp.AccessToken == "" {
		return "", fmt.Errorf("get access token failed: %s", tokenResp.ErrorDes)
	}

	cachedToken = tokenResp.AccessToken
	tokenExpire = time.Now().Add(tokenTTL)
	return cachedToken, nil
}
