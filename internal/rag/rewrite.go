// Package rag 的这部分负责"检索前的查询改写"。
package rag

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"deeptalk/internal/decision"

	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// 改写结果的分类，用于指标与排查。
const (
	RewriteSkipped       = "skipped"        // 不满足触发条件，未调用模型
	RewriteOK            = "rewritten"      // 成功改写
	RewriteFallbackEmpty = "fallback_empty" // 模型返回空 → 回退原句
	RewriteFallbackError = "fallback_error" // 调用失败 → 回退原句
	RewriteFallbackTime  = "fallback_timeout"
)

// shortQueryRunes 短问题阈值：短到没有实义关键词时，检索基本不会命中，
// 若还有历史就值得先消解一次（规格：< 8 字且存在历史）。
const shortQueryRunes = 8

// defaultRewriteTimeout 改写是"顺手多做一步"，不能拖慢主链路。
const defaultRewriteTimeout = 800 * time.Millisecond

// rewriteChatModel 只依赖"能生成"这一最小能力，方便测试替换。
type rewriteChatModel interface {
	Generate(ctx context.Context, input []*schema.Message, opts ...einomodel.Option) (*schema.Message, error)
}

// RewriteResult 一次改写的结果。Query 永远是可用的检索语句。
type RewriteResult struct {
	Query     string
	Rewritten bool
	Reason    string
}

// Rewriter 把依赖上下文的提问改写成自包含的检索语句。
//
// 存在的理由：意图层能识别"含指代词「它」"，但检索仍拿原句去查——
// 用户问"那它呢"，等于用"那它呢"做向量检索，召回必然接近随机。
// 这一步把指代替换成上文里的实体，补上的正是这段断层。
type Rewriter struct {
	llm     rewriteChatModel
	timeout time.Duration
}

func NewRewriter(llm rewriteChatModel) *Rewriter {
	return &Rewriter{llm: llm, timeout: defaultRewriteTimeout}
}

// ShouldRewrite 判断是否需要改写，并给出原因。
//
// 触发条件（满足任一）：
//   - 命中指代词：语义必然依赖上下文
//   - 问题过短且存在历史：多半是"那第二点呢"这类省略句
func ShouldRewrite(query string, historyRounds int) (bool, string) {
	if p, ok := decision.HasPronoun(query); ok {
		return true, "pronoun:" + p
	}
	if utf8.RuneCountInString(strings.TrimSpace(query)) < shortQueryRunes && historyRounds > 0 {
		return true, "short_with_history"
	}
	return false, ""
}

// Rewrite 尝试把 query 改写成自包含的检索语句。
//
// 任何失败（超时 / 报错 / 结果为空）都回退到原句——
// 改写是锦上添花，绝不能因为它挂了让整个问答失败。
func (r *Rewriter) Rewrite(ctx context.Context, query string, history []*schema.Message) RewriteResult {
	base := RewriteResult{Query: query, Reason: RewriteSkipped}
	if r == nil || r.llm == nil || strings.TrimSpace(query) == "" {
		return base
	}

	callCtx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	resp, err := r.llm.Generate(callCtx, buildRewritePrompt(query, history))
	if err != nil {
		base.Reason = RewriteFallbackError
		if errors.Is(err, context.DeadlineExceeded) || callCtx.Err() != nil {
			base.Reason = RewriteFallbackTime
		}
		return base
	}

	out := cleanRewriteOutput(resp)
	if out == "" {
		base.Reason = RewriteFallbackEmpty
		return base
	}
	return RewriteResult{Query: out, Rewritten: true, Reason: RewriteOK}
}

// buildRewritePrompt 组装改写提示。
//
// 约束写得比较死（只输出一句、不加解释、不扩展），
// 是因为这类"让模型顺手改一句话"的任务最容易被发挥成一段分析。
func buildRewritePrompt(query string, history []*schema.Message) []*schema.Message {
	var sb strings.Builder
	sb.WriteString("你的任务：把用户最后一句提问改写成**可以独立用于检索**的语句。\n\n")
	sb.WriteString("规则：\n")
	sb.WriteString("1. 把「它 / 这个 / 上面 / 刚才」等指代词替换成上文里对应的具体事物\n")
	sb.WriteString("2. 如果原句已经自包含、不需要改写，就原样输出\n")
	sb.WriteString("3. 只输出改写后的一句话，不要解释、不要加引号、不要 Markdown\n")
	sb.WriteString("4. 保持原意，不要扩展、不要补充原文没有的信息\n\n")

	if h := renderHistory(history); h != "" {
		sb.WriteString("【上文】\n")
		sb.WriteString(h)
		sb.WriteString("\n\n")
	}
	sb.WriteString("【用户最后一句】\n")
	sb.WriteString(query)
	sb.WriteString("\n\n【改写结果】")

	return []*schema.Message{{Role: schema.User, Content: sb.String()}}
}

// renderHistory 截取最近若干轮对话，避免把整个会话塞进改写提示。
func renderHistory(history []*schema.Message) string {
	const keep = 6 // 最近 3 轮
	if len(history) <= 1 {
		return ""
	}
	msgs := history[:len(history)-1] // 最后一条就是当前提问
	if len(msgs) > keep {
		msgs = msgs[len(msgs)-keep:]
	}

	var sb strings.Builder
	for _, m := range msgs {
		content := strings.TrimSpace(m.Content)
		if content == "" {
			continue
		}
		// 单条太长时截断：改写只需要知道"在说什么"，不需要全文
		if r := []rune(content); len(r) > 200 {
			content = string(r[:200]) + "…"
		}
		role := "用户"
		if m.Role == schema.Assistant {
			role = "助手"
		}
		fmt.Fprintf(&sb, "%s：%s\n", role, content)
	}
	return strings.TrimSpace(sb.String())
}

// cleanRewriteOutput 去掉模型爱加的那些包装。
func cleanRewriteOutput(resp *schema.Message) string {
	if resp == nil {
		return ""
	}
	out := strings.TrimSpace(resp.Content)
	out = strings.Trim(out, "\"'“”‘’`")
	out = strings.TrimSpace(out)
	// 极少数情况下模型还是吐了 JSON，兼容一下
	if strings.HasPrefix(out, "{") && strings.HasSuffix(out, "}") {
		var boxed struct {
			Query string `json:"query"`
		}
		if err := json.Unmarshal([]byte(out), &boxed); err == nil && strings.TrimSpace(boxed.Query) != "" {
			return strings.TrimSpace(boxed.Query)
		}
	}
	// 只要第一行：多行输出基本都是"解释 + 答案"的形态
	if i := strings.IndexByte(out, '\n'); i > 0 {
		out = strings.TrimSpace(out[:i])
	}
	return out
}
