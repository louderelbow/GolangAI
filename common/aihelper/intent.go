package aihelper

import (
	"context"
	"deeptalk/common/metrics"
	"deeptalk/config"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/cloudwego/eino-ext/components/model/openai"
	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// ======================== 意图识别（分层 + 置信度 + LLM 兜底） ========================
//
// 生产上的做法不是"训一个分类器"，而是分层路由，从便宜到贵逐层上抛：
//
//	L0 规则层（0ms / 0 成本）：加权词表 + 整词匹配 + 指代词守卫
//	   └─ 高置信度 → 直接返回（覆盖大部分流量）
//	   └─ 低置信度（模糊地带）→ 上抛 L1
//	L1 LLM 兜底层（只覆盖小部分流量）：用 function calling 让模型在候选意图里选一个，
//	   结构化输出、不会被自由文本污染；解析失败/未调用 → 回退默认意图（fail-safe）
//	结果缓存：同一问题短时间内只判一次，避免重复调用 LLM
//
// 设计原则：
//  1. 便宜的先做，贵的只兜底（LLM 占比要当指标盯）
//  2. 低置信度绝不猜——要么上抛，要么走安全的默认（question）
//  3. 每一次判定都留痕（intent/layer/score），这就是后续做数据闭环的原料

// Intent 意图类型
type Intent string

const (
	IntentSummary  Intent = "summary"  // 要求总结/概括整篇文档
	IntentQuestion Intent = "question" // 针对文档内容的具体提问（默认，走 RAG）
	IntentChat     Intent = "chat"     // 寒暄闲聊（跳过检索）
)

// 判定层
const (
	LayerRule    = "rule"    // 规则层直接命中
	LayerLLM     = "llm"     // 规则层不确定，LLM 兜底
	LayerDefault = "default" // LLM 兜底不可用/失败，走安全默认
)

// IntentResult 意图识别结果（带上来源与置信度，便于观测与排查）
type IntentResult struct {
	Intent Intent
	Score  int    // 规则层得分
	Layer  string // rule / llm / default
	Reason string
}

// intentLLM 只依赖"能绑工具 + 能生成"这一最小能力，方便测试替换
type intentLLM interface {
	Generate(ctx context.Context, input []*schema.Message, opts ...einomodel.Option) (*schema.Message, error)
	WithTools(tools []*schema.ToolInfo) (einomodel.ToolCallingChatModel, error)
}

// ---------------- 词表（加权，可用配置扩展） ----------------

// strongSummaryWords 强总结词：明确在要"整篇概括"
var strongSummaryWords = []string{"全文", "整篇文档", "整份文档", "整篇文章", "全部内容", "整体内容", "大致内容", "主要内容", "讲了什么"}

// weakSummaryWords 弱总结词：可能只是想问某个具体主题
// 例："总结一下员工放假几天" 实际是在问一个具体数字，应该走检索而不是整篇概括
var weakSummaryWords = []string{"总结", "概括", "摘要", "梳理一下"}

// chatWords 寒暄词（权重 1）。英文按整词匹配，避免 "hi" 命中 "this"。
var chatWords = []string{
	"你好", "您好", "谢谢", "多谢", "再见", "拜拜", "你是谁", "能做什么", "能干什么", "介绍一下你",
	"hello", "hi", "thanks",
}

// pronounWords 指代词：出现说明依赖上下文，绝不能判成"闲聊"
var pronounWords = []string{"它", "这个", "那个", "上面", "刚才", "前面", "这条", "该条", "上述"}

// identityWords 指向"助手自身身份"的元问题（应跳过文档检索，直接对话回答）
var identityWords = []string{
	"你是谁", "你是什么", "你叫什么", "你是ai", "你是机器人", "你是助手", "你是模型",
	"你是语言模型", "你是大模型", "你是rag", "你是mcp", "你是gpt", "你是deepseek",
	"你能做什么", "你能干什么", "你会什么", "你都会什么", "你支持什么",
}

// abilityWords 与"你会/你能"搭配时，属于询问助手能力（而非询问文档内容）
var abilityWords = []string{"画画", "画图", "写代码", "编程", "翻译", "唱歌", "语音", "识图", "图片", "数学"}

// taskWords 出现这些词说明用户在交代任务（在问文档，不是问助手自身）
var taskWords = []string{"帮我", "请帮", "查一下", "告诉我", "查找", "搜一下"}

// fillerWords 口语附和/确认词：只在很短的句子里才算闲聊（"嗯嗯好的我知道了"）
var fillerWords = []string{"嗯", "哦", "噢", "好的", "好嘞", "知道了", "收到", "行吧", "ok", "okay", "好的呢"}

// 规则层判定阈值
const (
	shortChatMaxRunes = 12 // 命中寒暄词且不超过这个长度 → 判定为闲聊
	shortFillerRunes  = 8  // 命中口语附和词且不超过这个长度 → 判定为闲聊
	veryShortRunes    = 4  // 极短且无词表命中 → 模糊，上抛 LLM
)

// ---------------- 规则层 ----------------

// ruleIntent 规则层判定
// 返回的 confident=false 表示"处于模糊地带"，应上抛 LLM 兜底
func ruleIntent(question string) (IntentResult, bool) {
	text := strings.TrimSpace(strings.ToLower(question))
	if text == "" {
		return IntentResult{Intent: IntentChat, Layer: LayerRule, Reason: "空问题"}, true
	}
	runes := utf8.RuneCountInString(text)

	// 守卫一：含指代词 → 一定是在指代上下文/文档内容，归为提问
	for _, p := range pronounWords {
		if strings.Contains(text, p) {
			return IntentResult{Intent: IntentQuestion, Score: 0, Layer: LayerRule, Reason: "含指代词「" + p + "」，依赖上下文"}, true
		}
	}

	// 规则一：强总结词（全文/整篇文档/讲了什么）→ 明确的"整篇概括"诉求
	for _, w := range strongSummaryWords {
		if strings.Contains(text, w) {
			return IntentResult{Intent: IntentSummary, Score: 2, Layer: LayerRule, Reason: "命中总结词「" + w + "」"}, true
		}
	}

	// 规则二：弱总结词（总结/概括…）——只有句子里没有具体疑问要素时才算"总结全文"
	// 例："总结一下员工放假几天"实际是在问一个具体数字，应该走检索而不是整篇概括
	for _, w := range weakSummaryWords {
		if strings.Contains(text, w) {
			if hasQuestionMarker(text) {
				break // 交给后面的疑问词守卫 → question（走 RAG，能精确定位到那一段）
			}
			return IntentResult{Intent: IntentSummary, Score: 2, Layer: LayerRule, Reason: "命中总结词「" + w + "」"}, true
		}
	}

	// 守卫二：元问题（问助手自身身份/能力）→ 闲聊，跳过文档检索
	// 必须放在疑问词守卫之前：否则"你是RAG模型吗""你会画画吗"会因为"吗"被当成文档提问
	if isMetaQuestion(text) {
		return IntentResult{Intent: IntentChat, Score: 1, Layer: LayerRule, Reason: "询问助手自身身份/能力"}, true
	}

	// 守卫三：疑问词/问号 → 是在提问，不能因为带了句"你好"就判成闲聊
	if hasQuestionMarker(text) {
		return IntentResult{Intent: IntentQuestion, Score: 0, Layer: LayerRule, Reason: "含疑问词或问号"}, true
	}

	// 规则三：口语附和/确认（很短的句子）→ 闲聊
	if runes <= shortFillerRunes {
		for _, w := range fillerWords {
			if matchWord(text, w) {
				return IntentResult{Intent: IntentChat, Score: 1, Layer: LayerRule, Reason: "口语附和词「" + w + "」"}, true
			}
		}
	}

	// 规则二：寒暄词命中
	for _, w := range chatWords {
		if !matchWord(text, w) {
			continue
		}
		if runes <= shortChatMaxRunes {
			return IntentResult{Intent: IntentChat, Score: 1, Layer: LayerRule, Reason: "命中寒暄词「" + w + "」且为短句"}, true
		}
		// 命中寒暄词但句子很长 → 模糊（可能是"你好，帮我看看这份制度里年假怎么算"）
		return IntentResult{Intent: IntentQuestion, Score: 1, Layer: LayerRule, Reason: "命中寒暄词但句子较长，需进一步判断"}, false
	}

	// 规则三：极短且无任何词表命中（"嗯"、"好的"、"?"）→ 模糊
	if runes <= veryShortRunes {
		return IntentResult{Intent: IntentChat, Score: 0, Layer: LayerRule, Reason: "极短句且无实义关键词"}, false
	}

	// 默认：具体提问（走 RAG），高置信度——正常提问不该每次都去调 LLM
	return IntentResult{Intent: IntentQuestion, Score: 0, Layer: LayerRule, Reason: "默认按内容提问处理"}, true
}

// questionMarkers 疑问词：出现即认为用户在提问（避免"你好请问年假几天"被判成闲聊）
var questionMarkers = []string{"吗", "呢", "多少", "多久", "怎么", "怎样", "如何", "为什么", "哪些", "哪个", "哪天", "几点", "几号", "几天", "几年", "是否", "能不能", "可不可以", "？", "?"}

func hasQuestionMarker(text string) bool {
	for _, m := range questionMarkers {
		if strings.Contains(text, m) {
			return true
		}
	}
	return false
}

// isMetaQuestion 判断是否是"问助手自身"的元问题
func isMetaQuestion(text string) bool {
	// 用户在交代任务时，即使带"你能"也是在问文档
	for _, t := range taskWords {
		if strings.Contains(text, t) {
			return false
		}
	}
	for _, w := range identityWords {
		if strings.Contains(text, w) {
			return true
		}
	}
	if strings.Contains(text, "你会") || strings.Contains(text, "你能") || strings.Contains(text, "你可以") {
		for _, a := range abilityWords {
			if strings.Contains(text, a) {
				return true
			}
		}
	}
	return false
}

// matchWord 英文按整词匹配（词边界），中文按子串匹配
func matchWord(text, word string) bool {
	if isASCIIWord(word) {
		idx := 0
		for {
			pos := strings.Index(text[idx:], word)
			if pos < 0 {
				return false
			}
			start := idx + pos
			end := start + len(word)
			if !isAlphaNumAt(text, start-1) && !isAlphaNumAt(text, end) {
				return true
			}
			idx = end
		}
	}
	return strings.Contains(text, word)
}

func isASCIIWord(s string) bool {
	for _, r := range s {
		if r > 127 {
			return false
		}
	}
	return true
}

func isAlphaNumAt(s string, i int) bool {
	if i < 0 || i >= len(s) {
		return false
	}
	c := s[i]
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// ---------------- LLM 兜底层（function calling） ----------------

func intentToolInfo() *schema.ToolInfo {
	return &schema.ToolInfo{
		Name: "set_intent",
		Desc: "给出用户问题的意图分类结果，必须调用本工具输出结论，不要用自然语言回答",
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"intent": {
				Type:     schema.String,
				Desc:     "summary=要求总结或概括整篇文档；chat=寒暄闲聊；question=针对文档内容的具体提问",
				Required: true,
			},
			"reason": {
				Type: schema.String,
				Desc: "一句话说明判断依据",
			},
		}),
	}
}

// llmIntent 用 function calling 做结构化意图分类
// hint 是规则层的初步判断，作为先验喂给模型（实测能减少模型"乱改"）
func llmIntent(ctx context.Context, q string, llm intentLLM, hint Intent) IntentResult {
	if llm == nil {
		return IntentResult{Intent: IntentQuestion, Layer: LayerDefault, Reason: "未配置兜底模型"}
	}

	bound, err := llm.WithTools([]*schema.ToolInfo{intentToolInfo()})
	if err != nil {
		return IntentResult{Intent: IntentQuestion, Layer: LayerDefault, Reason: "绑定工具失败: " + err.Error()}
	}

	const classifyPrompt = `你是意图分类器。请判断用户问题属于哪一类，并调用 set_intent 工具输出结论。

分类标准：
- summary：用户明确要求"总结/概括整篇文档"，例如"总结全文""概括整份文档""这份文档讲了什么"
- question：问题涉及文档内容——制度、流程、条款、数字、规定等。即使带了"你好""谢谢"等问候语，只要存在具体信息需求（几天/多少/怎么/流程/规定/条件），一律属于 question
- chat：与文档内容无关的寒暄、闲聊，或询问助手自身的身份与能力，例如"你好""谢谢""你是RAG模型吗"

注意：不要因为句子里有问候语就判成 chat，关键看有没有"针对文档的具体信息需求"。`

	resp, err := bound.Generate(ctx, []*schema.Message{
		{Role: schema.System, Content: classifyPrompt},
		{Role: schema.User, Content: fmt.Sprintf("用户问题：%s\n（规则层初步判断为 %s，请核对后给出结论）", q, hint)},
	})
	if err != nil {
		return IntentResult{Intent: IntentQuestion, Layer: LayerDefault, Reason: "LLM 调用失败: " + err.Error()}
	}

	for _, tc := range resp.ToolCalls {
		if tc.Function.Name != "set_intent" {
			continue
		}
		var args struct {
			Intent string `json:"intent"`
			Reason string `json:"reason"`
		}
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
			continue
		}
		switch Intent(strings.ToLower(strings.TrimSpace(args.Intent))) {
		case IntentSummary:
			return IntentResult{Intent: IntentSummary, Layer: LayerLLM, Reason: args.Reason}
		case IntentChat:
			return IntentResult{Intent: IntentChat, Layer: LayerLLM, Reason: args.Reason}
		case IntentQuestion:
			return IntentResult{Intent: IntentQuestion, Layer: LayerLLM, Reason: args.Reason}
		}
	}

	return IntentResult{Intent: IntentQuestion, Layer: LayerDefault, Reason: "模型未按要求调用工具"}
}

// ---------------- 缓存 ----------------

type intentCacheEntry struct {
	result  IntentResult
	expires time.Time
}

var (
	intentCacheMu sync.Mutex
	intentCache   = map[string]intentCacheEntry{}
)

func intentCacheGet(key string) (IntentResult, bool) {
	intentCacheMu.Lock()
	defer intentCacheMu.Unlock()
	e, ok := intentCache[key]
	if !ok || time.Now().After(e.expires) {
		if ok {
			delete(intentCache, key)
		}
		return IntentResult{}, false
	}
	return e.result, true
}

// resetIntentCache 清空意图缓存（测试用：缓存是全局状态，测试之间需要隔离）
func resetIntentCache() {
	intentCacheMu.Lock()
	defer intentCacheMu.Unlock()
	intentCache = map[string]intentCacheEntry{}
}

func intentCacheSet(key string, r IntentResult, ttl time.Duration) {
	cfg := config.GetConfig().IntentConfig
	max := cfg.MaxCacheEntries
	if max <= 0 {
		max = 1000
	}

	intentCacheMu.Lock()
	defer intentCacheMu.Unlock()
	if len(intentCache) >= max {
		// 简单的清空策略：容量满就重置（意图缓存本身是加速项，丢了不影响正确性）
		intentCache = map[string]intentCacheEntry{}
	}
	intentCache[key] = intentCacheEntry{result: r, expires: time.Now().Add(ttl)}
}

// ---------------- 对外入口 ----------------

func ClassifyIntent(ctx context.Context, question string, llm intentLLM) IntentResult {
	cfg := config.GetConfig().IntentConfig

	res, confident := ruleIntent(question)
	if confident || !cfg.LLMFallback {
		recordIntent(res)
		return res
	}
	// 缓存：同一问题短时间内只判一次
	cacheKey := strings.TrimSpace(strings.ToLower(question))
	ttl := time.Duration(cfg.CacheTTLSeconds) * time.Second
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	if cached, ok := intentCacheGet(cacheKey); ok {
		recordIntent(cached)
		return cached
	}

	final := llmIntent(ctx, question, llm, res.Intent)
	if final.Layer == LayerLLM {
		final.Score = res.Score
	}
	intentCacheSet(cacheKey, final, ttl)
	recordIntent(final)
	return final
}

func recordIntent(r IntentResult) {
	metrics.Count("deeptalk_intent_total", metrics.Labels{"intent": string(r.Intent), "layer": r.Layer}, 1)
	log.Printf("[Intent] intent=%s layer=%s score=%d reason=%s", r.Intent, r.Layer, r.Score, r.Reason)
}

// IntentMetricsName 供文档/看板引用的指标名
const IntentMetricsName = "deeptalk_intent_total"

func NewIntentLLM(ctx context.Context) (einomodel.ToolCallingChatModel, error) {
	cfg := config.GetConfig()
	key := cfg.RagModelConfig.RagApiKey
	if key == "" {
		key = os.Getenv("ALIYUN_API_KEY")
	}
	if key == "" {
		key = os.Getenv("DEEPSEEK_API_KEY")
	}
	if key == "" {
		key = os.Getenv("OPENAI_API_KEY")
	}
	return openai.NewChatModel(ctx, &openai.ChatModelConfig{
		BaseURL: cfg.RagModelConfig.RagBaseUrl,
		Model:   cfg.RagModelConfig.RagChatModelName,
		APIKey:  key,
	})
}

// RuleIntentOnly 只跑规则层，返回判定结果
func RuleIntentOnly(question string) IntentResult {
	res, _ := ruleIntent(question)
	return res
}

// RuleIntentWithConfidence 返回规则层结果与"是否高置信度"
func RuleIntentWithConfidence(question string) (IntentResult, bool) {
	return ruleIntent(question)
}

// AllIntents 全部候选意图（评测用）
func AllIntents() []Intent { return []Intent{IntentSummary, IntentQuestion, IntentChat} }

var _ = fmt.Sprintf // 保留 fmt 依赖（错误信息拼接用）
