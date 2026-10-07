package askuser

import (
	"context"
	"encoding/json"
	"log"
	"strings"
	"time"

	agenttool "deeptalk/internal/agent/tool"
	"deeptalk/internal/infra/metrics"

	"github.com/cloudwego/eino/schema"
)

// ToolName 工具名（轨迹、日志、前端都按这个名字识别）。
const ToolName = "ask_user"

// toolDescription 是提示词工程里最要紧的一段。
//
// 这个工具**滥用比不实现更糟**：每调用一次，用户就得停下来做一次选择，
// 这是他付出的成本。所以描述里的重心不是"什么时候该问"，而是
// "什么时候不该问"——默认动作用已有信息直接回答，而不是先问清楚再答。
//
// 判断标准收敛成一句话：**按最可能的那个答案回答，会不会让用户做错决定。**
// 会 → 问；只是答得粗一点 → 不问。
const toolDescription = `只在一种情况下调用本工具：按最可能的那个答案直接回答，会让用户做出错误的决定。

调用本工具意味着用户必须停下来做一次选择，这是他的成本。所以默认动作是"用已有信息直接回答"，
而不是"先问清楚再答"。

【以下情况一律不调用】
- 能给出通用答案的（例如"一般规定是…，具体以你们公司制度为准"）
- 只是答得不够细、不够贴合，但不会导致用户做错决定的
- 能从对话历史、用户资料、已检索到的内容里推断出来的
- 候选答案列不出 2~6 个互斥的明确选项（这种在回答正文里反问一句即可）
- 用户说过"直接说""别问我"这类意思
- 你自己也拿不准该不该问——拿不准就不要问

【三个条件必须同时满足】
1. 缺的这一点会实质改变答案：按默认值答等于"答错"，而不只是"答得粗"
2. 候选答案能枚举成 2~6 个互斥的明确选项
3. 无法从上下文推断，也不能用一次通用回答绕过去

【调用之后】
本轮立即结束，用户选完（或自己填一个）会带着补充信息继续。
因此调用前不要输出答案正文，也不要半答半问。

【示例】
用户：帮我请假
→ 不调用。请假的一般流程可以直接回答，用户追问细节时再澄清。

用户：你好
→ 不调用。

用户：这个怎么弄
→ 不调用。"这个"指什么列不出选项，在正文里反问一句即可。

用户：公司年假多少天
→ 不调用。信息足够，直接依据制度回答。

用户：帮我写个 SQL，取每个部门薪水最高的员工
→ 调用。question="你用的是哪种数据库？"
  options=[MySQL, PostgreSQL, SQL Server, SQLite]
  理由：方言不同写法不同，给错了用户直接跑不起来

用户：报销要走什么流程
→ 调用。question="这笔报销金额大概多少？"
  options=[500 元以内, 500-5000 元, 5000 元以上]
  理由：三档的审批人完全不同，答错用户会走错流程`

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
		Idempotent: false,
		// 本工具靠"返回哨兵错误"来中断 ReAct 循环，所以这个错误是**正常语义**，
		// 不是失败：不能进熔断器（连着澄清几次会把 breaker 打开、之后再也弹不出来），
		// 也不能被回填成 observation（那样模型会以为工具坏了，转而自己编答案）。
		InterruptOnError: true,
		Timeout:          2 * time.Second, // 纯内存操作，慢了说明有问题
		RetryPolicy:      agenttool.RetryPolicy{MaxAttempts: 1},
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
	Question string `json:"question"`
	Reason   string `json:"reason"`
	// 用 RawMessage 而不是 []Option：模型的参数形态很不稳定，
	// 直接反序列化成结构体会让**整个参数**解析失败——连 question 都拿不到，
	// 一次本来能成立的追问就被整条丢掉了。这里把 options 单独降级解析。
	Options json.RawMessage `json:"options"`
}

// parseOptions 尽最大努力把模型给的 options 解析成选项列表。
//
// 实测过的真实形态（qwen 系列）：
//
//	[{"id":"annual","label":"年假"}]   ← 标准
//	["年假","病假"]                     ← 只给字符串数组，很常见
//	[{"value":"年假"},{"value":"病假"}] ← 字段名不是 label
//
// 都会在这里被收成统一的 Option；Normalize 再补 id、去重、截断。
func parseOptions(raw json.RawMessage) []Option {
	if len(raw) == 0 {
		return nil
	}

	var objs []Option
	if err := json.Unmarshal(raw, &objs); err == nil {
		// 全部没有 label，说明字段名不是 label（例如 value/text）：
		// 这一支解析"成功"了但没拿到东西，交给下面的兜底再试一次
		for _, o := range objs {
			if strings.TrimSpace(o.Label) != "" {
				return objs
			}
		}
	}

	var strs []string
	if err := json.Unmarshal(raw, &strs); err == nil {
		out := make([]Option, 0, len(strs))
		for _, s := range strs {
			out = append(out, Option{Label: s})
		}
		return out
	}

	// 最后兜底：对象里的键名千奇百怪，取第一个非空字符串当 label
	var loose []map[string]any
	if err := json.Unmarshal(raw, &loose); err == nil {
		out := make([]Option, 0, len(loose))
		for _, m := range loose {
			for _, key := range []string{"label", "text", "value", "name", "title"} {
				if s, ok := m[key].(string); ok && strings.TrimSpace(s) != "" {
					out = append(out, Option{Label: s})
					break
				}
			}
		}
		return out
	}

	return nil
}

func handler(ctx context.Context, argsJSON string) (string, error) {
	var args toolArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		// 参数是模型生成的，格式坏掉是它的问题，不该让整轮失败——
		// 否则用户会拿到一个"模型运行失败"，而他只是想问个问题。
		// 回填一句可操作的观察，让它自己决定重试还是直接作答。
		//
		// 打日志是因为这种失败**对用户完全不可见**：模型往往就此改口，
		// 把问题和选项写进正文交差，用户看到的是一条普通回复，
		// 谁也想不到澄清能力其实被一次坏参数吞掉了。
		log.Printf("[ask_user] 参数不是合法 JSON，本轮放弃澄清 args=%.200s", argsJSON)
		return "提问未发出：参数格式不正确。" +
			"请直接回答用户，或重新调用本工具，参数为 {question, reason, options:[{id,label,hint}]}。", nil
	}

	req := Request{Question: args.Question, Options: parseOptions(args.Options), Reason: args.Reason}
	req.Normalize()

	if !req.Valid() {
		// 参数不合法时不中断整轮：模型把选项写少了是它的问题，
		// 不该让用户因此什么回答都拿不到。回填一句观察，让它自己决定
		// 是直接作答还是重新提问。
		log.Printf("[ask_user] 参数不合法，本轮放弃澄清 question=%.40q options=%d args=%.200s",
			req.Question, len(req.Options), argsJSON)
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
