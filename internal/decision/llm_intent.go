package decision

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/schema"
)

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
