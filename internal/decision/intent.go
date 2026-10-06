package decision

import (
	"context"
	"strings"
	"unicode/utf8"

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
