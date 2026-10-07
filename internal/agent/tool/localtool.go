package tool

import (
	"context"
	"errors"
	"time"

	"github.com/cloudwego/eino/schema"
)

// errLocalAgentDisabled 只在装配出错时出现（没给转发函数却注册了工具）。
var errLocalAgentDisabled = errors.New("本地工作区能力未启用")

// LocalForwarder 把一次工具调用转发给该用户已连接的本地 agent。
//
// 用函数类型而不是直接依赖 internal/agent/local：工具层不需要知道这条链路
// 是 WebSocket 还是别的什么，它只需要"把这段 JSON 送过去、拿一段文本回来"。
// 谁来提供这个转发（哪个用户的哪条连接）是装配层的事。
type LocalForwarder func(ctx context.Context, tool, argsJSON string) (string, error)

// 这两段描述是提示词的一部分：模型据此决定要不要去翻用户的文件、
// 以及翻不到时该怎么回答。重点写三件事——去哪里找、怎么找、
// 找不到时不要编。
const (
	listFilesDesc = `列出**用户自己电脑上**当前工作区里某个目录的内容。

path 是相对工作区根的路径（根目录写 "."）；也可以直接给工作区内的绝对路径。
返回每条目的类型（d 目录 / l 符号链接 / - 普通文件）、大小、修改时间与名称。

不确定项目结构时，先用它看 "."，再逐层深入；不要凭猜测直接读某个路径。`

	readFileDesc = `读取**用户自己电脑上**当前工作区里某个文件的内容。

path 是相对工作区根的路径；用户直接给了完整路径时，照抄那个绝对路径也可以，
只要文件在工作区内。工作区之外的路径会被拒绝，不要反复尝试。
只读文本文件；二进制文件会返回一句提示而不是乱码。
文件过大时会被截断并在末尾注明，这时应当去读别的文件，而不是反复重试同一个。

回答涉及文件内容时，请说明来自哪个文件；文件里没有的东西不要凭印象补充。`
)

// workspaceParam 是本地这几个工具共用的唯一参数。
func workspaceParam(desc string) map[string]*schema.ParameterInfo {
	return map[string]*schema.ParameterInfo{
		"path": {Type: schema.String, Desc: desc, Required: true},
	}
}

// LocalWorkspaceTools 返回查看**用户自己电脑上工作区**的工具集。
//
// 为什么只有"看"没有"改"：读文件幂等、无副作用，错了也只是浪费一次调用；
// 写文件与执行命令不是。后者需要一个用户在自己电脑上点"允许"的确认机制，
// 而那属于另一件事——先只做只读，是为了把安全边界先立起来：
// **本地那端才是唯一能碰硬盘的地方，服务端只能请求。**
//
// 这几个工具的价值不在于"能读文件"，而在于路径解析与越界拒绝发生在
// 用户自己的机器上——服务端即使被攻破，也只能请求，不能强制。
func LocalWorkspaceTools(forward LocalForwarder) []ToolSpec {
	return []ToolSpec{
		{
			Name:        "list_files",
			Description: listFilesDesc,
			Info: &schema.ToolInfo{
				Name:        "list_files",
				Desc:        listFilesDesc,
				ParamsOneOf: schema.NewParamsOneOfByParams(workspaceParam(`相对工作区根的目录路径，根目录写 "."`)),
			},
			Handler:    forwardTo(forward, "list_files"),
			Idempotent: true,
		},
		{
			Name:        "read_file",
			Description: readFileDesc,
			Info: &schema.ToolInfo{
				Name:        "read_file",
				Desc:        readFileDesc,
				ParamsOneOf: schema.NewParamsOneOfByParams(workspaceParam(`相对工作区根的文件路径`)),
			},
			Handler:    forwardTo(forward, "read_file"),
			Idempotent: true,
			Timeout:    20 * time.Second, // 比默认略宽：大文件 + 一次网络往返
		},
	}
}

// forwardTo 把参数原样转给本地 agent。
//
// 服务端不做任何本地解析：参数的含义由本地那端定义，多解析一次
// 就多一处可能与本地实现不一致的地方。
func forwardTo(forward LocalForwarder, toolName string) InvokeFunc {
	return func(ctx context.Context, argsJSON string) (string, error) {
		if forward == nil {
			return "", errLocalAgentDisabled
		}
		return forward(ctx, toolName, argsJSON)
	}
}
