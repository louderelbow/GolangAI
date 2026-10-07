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
	Label    string // 用户选了哪一项
}

// augmentQuestion 把用户对澄清提问的选择拼成补充信息。
//
// 拼在问题原文之后而不是替换它：模型既能看到用户最初是怎么问的，
// 也能看到澄清之后的具体所指——两段信息对回答都有用。
//
// 用带「」的自然语言而不是结构化 JSON：这段文本会进对话历史，
// 用户回看历史时应当能读懂，而不是看到一串字段。
func augmentQuestion(question string, cl *ClarifyContext) string {
	if cl == nil {
		return question
	}
	label := strings.TrimSpace(cl.Label)
	if label == "" {
		return question
	}
	if q := strings.TrimSpace(cl.Question); q != "" {
		return fmt.Sprintf("%s\n（补充信息：针对「%s」，用户选择了「%s」）", question, q, label)
	}
	return fmt.Sprintf("%s\n（补充信息：用户选择了「%s」）", question, label)
}
