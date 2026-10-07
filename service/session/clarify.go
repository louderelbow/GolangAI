package session

import (
	"fmt"
	"strings"
)

// ClarifyContext 用户在上一轮澄清提问里做出的选择。
//
// 定义在这里而不是直接复用 controller 的 DTO：service 不该反向依赖 controller。
type ClarifyContext struct {
	Question string // 上一轮问的是什么
	Label    string // 用户选的那一项；Custom=true 时是用户自己输入的原文
	Custom   bool   // 用户没点选项，而是自己填了一个
}

// augmentQuestion 把用户对澄清提问的回应拼成补充信息。
//
// 拼在问题原文之后而不是替换它：模型既能看到用户最初是怎么问的，
// 也能看到澄清之后的具体所指——两段信息对回答都有用。
//
// 用带「」的自然语言而不是结构化 JSON：这段文本会进对话历史，
// 用户回看历史时应当能读懂，而不是看到一串字段。
//
// 点选项和自己填要分开措辞：前者是"在给定范围里挑一个"，
// 后者是"你给的选项都不对，我来说"。混成同一句话会让模型
// 误以为用户认可了预设的框架，这对开放式回答是误导。
func augmentQuestion(question string, cl *ClarifyContext) string {
	if cl == nil {
		return question
	}
	answer := strings.TrimSpace(cl.Label)
	if answer == "" {
		return question
	}

	verb := "用户选择了"
	if cl.Custom {
		verb = "用户补充说明"
	}

	if q := strings.TrimSpace(cl.Question); q != "" {
		return fmt.Sprintf("%s\n（补充信息：针对「%s」，%s「%s」）", question, q, verb, answer)
	}
	return fmt.Sprintf("%s\n（补充信息：%s「%s」）", question, verb, answer)
}
