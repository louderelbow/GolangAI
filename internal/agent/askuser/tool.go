package askuser

import (
	"context"
	"encoding/json"
	"log"
	"time"

	agenttool "deeptalk/internal/agent/tool"
	"deeptalk/internal/infra/metrics"

	"github.com/cloudwego/eino/schema"
)

// ToolName 工具名（轨迹、日志、前端都按这个名字识别）。
const ToolName = "ask_user"

// toolDescription 是提示词工程里最要紧的一段。
//
// 这个工具**滥用比不实现更糟**：如果模型动不动就弹选择框，
// 用户会觉得它什么都不肯自己答。所以描述里把"不要调用"的场景
// 写得比"要调用"还具体，并且给出正反例。
const toolDescription = `当用户的问题缺少做出准确回答所必需的关键信息，且这些信息可以归纳为有限的几个明确选项时，向用户提问并让用户选择。

【同时满足以下三点才调用】
1. 缺少的信息会显著改变答案——猜错的代价是"答错"而不只是"答得不够细"
2. 答案能枚举成 2~6 个明确选项
3. 从对话历史、用户资料、已检索到的内容里都推不出来

【以下情况不要调用】
- 信息不全但可以给出通用回答（例如"一般规定是…，具体以你们公司制度为准"）
- 选项是开放式的、无法枚举（这时应该在回答正文里反问，而不是用本工具）
- 只是想让用户确认你已知或已能推断的事情
- 用户表达了"直接说""别问我"这类意思

【调用之后】
本轮立即结束，用户选择后会带着补充信息继续。因此调用前不要输出答案正文，也不要半答半问。

【示例】
用户：帮我请假
→ 调用。question="你要请哪种假？"，options=[年假, 病假, 事假, 调休]
  理由：三种假的规则完全不同，随便挑一个回答就是编造

用户：公司年假多少天
→ 不调用。信息足够，直接依据制度回答

用户：这个怎么弄
→ 不调用。"这个"指什么无法枚举成选项，应在回答里反问`

// Tool 返回 ask_user 的工具描述，供工具注册表装载。
//
// 只有统一 Agent（modelType 6）会注册它：RAG 与 DeepSeek 是单轮直答路径，
// 那里没有"停下来等用户"的语义，注入了也没人会去读收集器。
func Tool() agenttool.ToolSpec {
	return agenttool.ToolSpec{
		Name:        ToolName,
		Description: toolDescription,
		Info:        toolInfo(),
		Handler:     handler,
		// 同一轮里问两次只会让用户困惑；Set 也只保留第一次
		Idempotent:  false,
		Timeout:     2 * time.Second, // 纯内存操作，慢了说明有问题
		RetryPolicy: agenttool.RetryPolicy{MaxAttempts: 1},
	}
}

func toolInfo() *schema.ToolInfo {
	return &schema.ToolInfo{
		Name: ToolName,
		Desc: toolDescription,
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"question": {
				Type:     schema.String,
				Desc:     "向用户提出的问题，一句话说清你要问什么",
				Required: true,
			},
			"reason": {
				Type: schema.String,
				Desc: "为什么必须问清楚（会展示给用户，帮助他理解你为什么要打断）",
			},
			"options": {
				Type: schema.Array,
				Desc: "可选项，2~6 个，覆盖用户最可能的情况",
				ElemInfo: &schema.ParameterInfo{
					Type: schema.Object,
					SubParams: map[string]*schema.ParameterInfo{
						"id": {
							Type: schema.String,
							Desc: "选项标识，简短英文或拼音，如 annual_leave",
						},
						"label": {
							Type:     schema.String,
							Desc:     "用户看到的选项文字，如「年假」",
							Required: true,
						},
						"hint": {
							Type: schema.String,
							Desc: "可选的一句说明，如「入职满一年可休」",
						},
					},
				},
			},
		}),
	}
}

type toolArgs struct {
	Question string   `json:"question"`
	Reason   string   `json:"reason"`
	Options  []Option `json:"options"`
}

func handler(ctx context.Context, argsJSON string) (string, error) {
	var args toolArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		// 参数是模型生成的，格式坏掉是它的问题，不该让整轮失败——
		// 否则用户会拿到一个"模型运行失败"，而他只是想问个问题。
		// 回填一句可操作的观察，让它自己决定重试还是直接作答。
		return "提问未发出：参数格式不正确。" +
			"请直接回答用户，或重新调用本工具，参数为 {question, reason, options:[{id,label,hint}]}。", nil
	}

	req := Request{Question: args.Question, Options: args.Options, Reason: args.Reason}
	req.Normalize()

	if !req.Valid() {
		// 参数不合法时不中断整轮：模型把选项写少了是它的问题，
		// 不该让用户因此什么回答都拿不到。回填一句观察，让它自己决定
		// 是直接作答还是重新提问。
		return "提问未发出：问题为空或有效选项少于 2 个。" +
			"请直接回答用户，或重新调用本工具并给出 2~6 个明确选项。", nil
	}

	collector, ok := FromContext(ctx)
	if !ok {
		// 没有收集器说明当前路径不支持与用户交互（例如非 Agent 模型）
		log.Printf("[ask_user] 未找到本轮收集器，降级为直接回答")
		return "提问未发出：当前无法与用户交互，请直接回答。", nil
	}

	collector.Set(req)
	metrics.CountAgentAskUser()

	// 返回哨兵错误来中断本轮：eino 的 ReAct 在工具报错时会结束循环。
	//
	// 注意这里只是"让本轮停下来"，不是失败——收集器里的内容由**服务层**读取，
	// Agent 核心不碰它。两边都读的话，先读的那个会把内容取走，
	// 后读的就只能拿到空值（这个坑实际踩过一次）。
	return "", ErrRequested
}
