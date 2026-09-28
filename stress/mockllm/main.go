// mockllm 是一个 OpenAI 兼容的假模型服务，专门用于给 DeepTalk 做压测。
//
// 为什么压测一定要先 mock 上游：
//   - 直接压真实模型，测出来的其实是上游的限流和排队，不是你系统的吞吐；
//   - 真实调用要花钱，而且并发一高就被限流，压不到你自己的瓶颈。
//
// 它同时提供 chat/completions 和 embeddings 两个端点，因为 DeepTalk 里
// RAG 问答（检索用的 embedding）、意图识别的 LLM 兜底、语义缓存都指向同一个 baseUrl，
// 改一个配置就能全部指向这里。
//
// 用法：
//
//	go run ./stress/mockllm -addr :8099 -mode ok
//
// 运行时切换故障模式（不用重启）：
//
//	curl "http://127.0.0.1:8099/__admin/mode?mode=500"
//	curl "http://127.0.0.1:8099/__admin/mode?mode=slow"
//	curl "http://127.0.0.1:8099/__admin/mode?mode=flaky"
//	curl "http://127.0.0.1:8099/__admin/mode?mode=ok"
//	curl  "http://127.0.0.1:8099/__admin/stats"
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math"
	"math/rand"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

type mode string

const (
	modeOK    mode = "ok"    // 正常返回（带人为延迟，模拟真实推理耗时）
	mode500   mode = "500"   // 全部返回 500，用于验证熔断
	modeSlow  mode = "slow"  // 慢响应（延迟 × slowFactor），用于验证超时与排队
	modeFlaky mode = "flaky" // 按 failRate 概率返回 500，用于验证熔断的失败率触发
)

type server struct {
	mu   sync.RWMutex
	mode mode

	delay       time.Duration // 每次请求的人为延迟（模拟推理耗时）
	slowFactor  float64       // slow 模式下的延迟倍数
	failRate    float64       // flaky 模式的失败概率
	dim         int           // embedding 维度，必须和 config 里的 ragModelConfig.dimension 一致
	cachedRatio float64       // 上报的 cache 命中比例（1.0 = 全部输入 token 都算命中）
	replyRunes  int           // 回复长度（字符数）
	toolCall    string        // 非空时总是返回一次工具调用（用于打满 ReAct 的 MaxStep）

	started time.Time
	stats   stats
}

type stats struct {
	chatReqs         atomic.Int64
	embedReqs        atomic.Int64
	embedTexts       atomic.Int64
	failed           atomic.Int64
	injectedFaults   atomic.Int64
	promptTokens     atomic.Int64
	completionTokens atomic.Int64
	cachedTokens     atomic.Int64
}

// ---------------- chat completions ----------------

type chatRequest struct {
	Model    string            `json:"model"`
	Messages []chatMessage     `json:"messages"`
	Stream   bool              `json:"stream"`
	Tools    []json.RawMessage `json:"tools"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

func (s *server) handleChat(w http.ResponseWriter, r *http.Request) {
	s.stats.chatReqs.Add(1)

	// 故障注入先于一切：让熔断器尽快跳闸
	if s.shouldFail() {
		s.stats.failed.Add(1)
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": map[string]any{"message": "mock upstream failure", "type": "mock_error"},
		})
		return
	}

	var req chatRequest
	body := readBody(r)
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]any{"message": "bad request: " + err.Error()},
		})
		return
	}

	s.sleep()

	promptTokens := 0
	for _, m := range req.Messages {
		promptTokens += countTokens(m.Content)
	}
	usage := s.buildUsage(promptTokens)
	s.stats.promptTokens.Add(int64(usage.PromptTokens))
	s.stats.completionTokens.Add(int64(usage.CompletionTokens))
	s.stats.cachedTokens.Add(int64(usage.promptDetails.CachedTokens))

	if req.Stream {
		s.streamChat(w, req, usage)
		return
	}

	content := s.reply()
	msg := map[string]any{"role": "assistant", "content": content}
	resp := map[string]any{
		"id":      newID(),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   orDefault(req.Model, "mock-model"),
		"choices": []any{map[string]any{
			"index":         0,
			"message":       msg,
			"finish_reason": "stop",
		}},
		"usage": usage.toMap(),
	}
	// 带工具时返回一次 tool_call，用于验证 ReAct 循环 / MaxStep 行为
	if tc := s.buildToolCall(req); tc != nil {
		msg["content"] = ""
		msg["tool_calls"] = []any{tc}
		resp["choices"].([]any)[0].(map[string]any)["finish_reason"] = "tool_calls"
	}

	writeJSON(w, http.StatusOK, resp)
}

// streamChat 按 OpenAI SSE 协议逐块吐出内容。
// 关键点：Eino 的 openai 组件会把 stream_options.include_usage 设为 true，
// 所以最后必须再发一个只带 usage、choices 为空的 chunk，否则上游用量统计拿不到数据。
func (s *server) streamChat(w http.ResponseWriter, req chatRequest, usage *tokenUsage) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	flusher, ok := w.(http.Flusher)
	if !ok {
		log.Printf("[mock] response writer does not support flushing")
		return
	}

	id := newID()
	modelName := orDefault(req.Model, "mock-model")
	content := s.reply()
	runes := []rune(content)

	const chunkRunes = 12
	for i := 0; i < len(runes); i += chunkRunes {
		end := i + chunkRunes
		if end > len(runes) {
			end = len(runes)
		}
		chunk := map[string]any{
			"id":      id,
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"model":   modelName,
			"choices": []any{map[string]any{
				"index":         0,
				"delta":         map[string]any{"content": string(runes[i:end])},
				"finish_reason": nil,
			}},
		}
		writeSSE(w, chunk)
		flusher.Flush()
	}

	// 结束块
	writeSSE(w, map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   modelName,
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         map[string]any{},
			"finish_reason": "stop",
		}},
	})
	// usage 块（choices 为空）
	writeSSE(w, map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   modelName,
		"choices": []any{},
		"usage":   usage.toMap(),
	})
	fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}

func (s *server) buildToolCall(req chatRequest) map[string]any {
	s.mu.RLock()
	name := s.toolCall
	s.mu.RUnlock()
	if name == "" || len(req.Tools) == 0 {
		return nil
	}
	return map[string]any{
		"id":   newID(),
		"type": "function",
		"function": map[string]any{
			"name":      name,
			"arguments": "{}",
		},
	}
}

// ---------------- embeddings ----------------

type embedRequest struct {
	Model string          `json:"model"`
	Input json.RawMessage `json:"input"`
}

type embedResponse struct {
	Embedding []float32 `json:"embedding"`
}

func (s *server) handleEmbeddings(w http.ResponseWriter, r *http.Request) {
	s.stats.embedReqs.Add(1)

	if s.shouldFail() {
		s.stats.failed.Add(1)
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": map[string]any{"message": "mock upstream failure", "type": "mock_error"},
		})
		return
	}

	var req embedRequest
	if err := json.Unmarshal(readBody(r), &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]any{"message": "bad request: " + err.Error()},
		})
		return
	}
	texts := parseEmbedInput(req.Input)
	s.stats.embedTexts.Add(int64(len(texts)))

	s.sleep()

	s.mu.RLock()
	dim := s.dim
	s.mu.RUnlock()

	data := make([]map[string]any, 0, len(texts))
	promptTokens := 0
	for i, t := range texts {
		promptTokens += countTokens(t)
		data = append(data, map[string]any{
			"object":    "embedding",
			"index":     i,
			"embedding": fakeVector(t, dim),
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"id":      newID(),
		"object":  "list",
		"created": time.Now().Unix(),
		"model":   orDefault(req.Model, "mock-embedding"),
		"data":    data,
		"usage": map[string]any{
			"prompt_tokens":     promptTokens,
			"completion_tokens": 0,
			"total_tokens":      promptTokens,
		},
	})
}

// parseEmbedInput input 既可能是字符串，也可能是字符串数组
func parseEmbedInput(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var one string
	if err := json.Unmarshal(raw, &one); err == nil {
		return []string{one}
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err == nil {
		return many
	}
	return nil
}

// fakeVector 生成一个确定性的单位向量：
// 同样的文本永远得到同样的向量，所以 Redis 向量检索的结果稳定、可复现。
func fakeVector(text string, dim int) []float32 {
	if dim <= 0 {
		dim = 1024
	}
	seed := int64(0)
	for _, b := range []byte(text) {
		seed = seed*31 + int64(b)
	}
	rnd := rand.New(rand.NewSource(seed))

	vec := make([]float32, dim)
	var norm float64
	for i := range vec {
		v := rnd.NormFloat64()
		vec[i] = float32(v)
		norm += v * v
	}
	norm = math.Sqrt(norm)
	if norm == 0 {
		norm = 1
	}
	for i := range vec {
		vec[i] = float32(float64(vec[i]) / norm)
	}
	return vec
}

// ---------------- usage ----------------

type tokenUsage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	promptDetails    promptDetails
}

type promptDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

func (s *server) buildUsage(promptTokens int) *tokenUsage {
	s.mu.RLock()
	cachedRatio := s.cachedRatio
	s.mu.RUnlock()

	cached := int(float64(promptTokens) * cachedRatio)
	if cached < 0 {
		cached = 0
	}
	if cached > promptTokens {
		cached = promptTokens
	}
	completion := s.replyRunesCount()
	return &tokenUsage{
		PromptTokens:     promptTokens,
		CompletionTokens: completion,
		TotalTokens:      promptTokens + completion,
		promptDetails:    promptDetails{CachedTokens: cached},
	}
}

func (u *tokenUsage) toMap() map[string]any {
	return map[string]any{
		"prompt_tokens":     u.PromptTokens,
		"completion_tokens": u.CompletionTokens,
		"total_tokens":      u.TotalTokens,
		// 这个字段是 DeepTalk 观测"上游前缀缓存命中了多少 token"的唯一来源
		"prompt_tokens_details": map[string]any{
			"cached_tokens": u.promptDetails.CachedTokens,
		},
	}
}

func (s *server) replyRunesCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.replyRunes
}

func (s *server) reply() string {
	n := s.replyRunesCount()
	if n <= 0 {
		n = 64
	}
	const unit = "本制度适用于公司全体正式员工，试用期员工参照执行。"
	runes := make([]rune, 0, n)
	for len(runes) < n {
		runes = append(runes, []rune(unit)...)
	}
	return string(runes[:n])
}

// ---------------- 故障注入与延迟 ----------------

func (s *server) shouldFail() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	switch s.mode {
	case mode500:
		return true
	case modeFlaky:
		if rand.Float64() < s.failRate {
			s.stats.injectedFaults.Add(1)
			return true
		}
	}
	return false
}

func (s *server) sleep() {
	s.mu.RLock()
	d := s.delay
	m := s.mode
	f := s.slowFactor
	s.mu.RUnlock()

	if m == modeSlow {
		d = time.Duration(float64(d) * f)
	}
	if d > 0 {
		time.Sleep(d)
	}
}

// ---------------- admin ----------------

func (s *server) handleMode(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("mode")
	if q == "" {
		q = r.FormValue("mode")
	}
	if q != "" {
		switch mode(q) {
		case modeOK, mode500, modeSlow, modeFlaky:
			s.mu.Lock()
			s.mode = mode(q)
			s.mu.Unlock()
			log.Printf("[mock] mode -> %s", q)
		default:
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unknown mode: " + q})
			return
		}
	}
	s.mu.RLock()
	cur := s.mode
	s.mu.RUnlock()
	writeJSON(w, http.StatusOK, map[string]any{"mode": cur})
}

func (s *server) handleStats(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	cur := s.mode
	s.mu.RUnlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"mode":              cur,
		"uptime_seconds":    time.Since(s.started).Seconds(),
		"chat_requests":     s.stats.chatReqs.Load(),
		"embed_requests":    s.stats.embedReqs.Load(),
		"embed_texts":       s.stats.embedTexts.Load(),
		"failed_requests":   s.stats.failed.Load(),
		"injected_faults":   s.stats.injectedFaults.Load(),
		"prompt_tokens":     s.stats.promptTokens.Load(),
		"completion_tokens": s.stats.completionTokens.Load(),
		"cached_tokens":     s.stats.cachedTokens.Load(),
	})
}

func (s *server) handleReset(w http.ResponseWriter, r *http.Request) {
	s.stats = stats{}
	s.mu.Lock()
	s.mode = modeOK
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "mode": modeOK})
}

// ---------------- helpers ----------------

func (s *server) postOnly(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "POST only"})
			return
		}
		h(w, r)
	}
}

func readBody(r *http.Request) []byte {
	defer r.Body.Close()
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 4096)
	for {
		n, err := r.Body.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
		}
		if err != nil {
			break
		}
		// 压测下不允许无上限读取，超过 8MB 直接截断（真实请求远小于此）
		if len(buf) > 8<<20 {
			break
		}
	}
	return buf
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeSSE(w http.ResponseWriter, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "data: %s\n\n", b)
}

func newID() string {
	return fmt.Sprintf("mock-%d-%d", time.Now().UnixNano(), rand.Int63n(100000))
}

func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

// countTokens 与项目里 compressor.countTokens 的估算口径保持一致，
// 这样压测看到的 token 数和线上日志可比。
func countTokens(text string) int {
	asciiCount := 0
	for _, r := range text {
		if r < 128 {
			asciiCount++
		}
	}
	nonAscii := utf8.RuneCountInString(text) - asciiCount
	return (asciiCount+3)/4 + (nonAscii+1)/2
}

func main() {
	addr := flag.String("addr", ":8099", "监听地址")
	m := flag.String("mode", string(modeOK), "初始模式：ok / 500 / slow / flaky")
	delay := flag.Duration("delay", 200*time.Millisecond, "每次请求的人为延迟（模拟推理耗时）")
	slowFactor := flag.Float64("slowFactor", 5, "slow 模式下的延迟倍数")
	failRate := flag.Float64("failRate", 0.5, "flaky 模式的失败概率")
	dim := flag.Int("dim", 1024, "embedding 维度，必须与 ragModelConfig.dimension 一致")
	cachedRatio := flag.Float64("cachedRatio", 0.8, "上报的前缀缓存命中比例（0~1）")
	replyRunes := flag.Int("replyRunes", 80, "回复字符数")
	toolCall := flag.String("toolCall", "", "非空则总是返回该名字的工具调用（用于打满 ReAct MaxStep）")
	flag.Parse()

	switch mode(*m) {
	case modeOK, mode500, modeSlow, modeFlaky:
	default:
		log.Fatalf("[mock] 未知模式 %q，可选：ok / 500 / slow / flaky", *m)
	}

	s := &server{
		mode:        mode(*m),
		delay:       *delay,
		slowFactor:  *slowFactor,
		failRate:    *failRate,
		dim:         *dim,
		cachedRatio: *cachedRatio,
		replyRunes:  *replyRunes,
		toolCall:    *toolCall,
		started:     time.Now(),
	}

	mux := http.NewServeMux()
	// 同时挂 /v1/... 和裸路径，避免 baseUrl 末尾要不要带 /v1 的问题
	mux.HandleFunc("/v1/chat/completions", s.postOnly(s.handleChat))
	mux.HandleFunc("/chat/completions", s.postOnly(s.handleChat))
	mux.HandleFunc("/v1/embeddings", s.postOnly(s.handleEmbeddings))
	mux.HandleFunc("/embeddings", s.postOnly(s.handleEmbeddings))

	mux.HandleFunc("/__admin/mode", s.handleMode)
	mux.HandleFunc("/__admin/stats", s.handleStats)
	mux.HandleFunc("/__admin/reset", s.handleReset)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok")
	})

	srv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	log.Printf("[mock] listening on %s mode=%s delay=%s dim=%d cachedRatio=%.2f",
		*addr, *m, *delay, *dim, *cachedRatio)
	log.Printf("[mock] 把 config.toml 的 ragModelConfig.baseUrl 指到 http://127.0.0.1%s/v1", *addr)
	if err := srv.ListenAndServe(); err != nil {
		log.Printf("[mock] server exit: %v", err)
		os.Exit(1)
	}
}
