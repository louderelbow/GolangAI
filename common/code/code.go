package code

//code响应状态码

type Code int64

const (
	CodeSuccess Code = 1000

	CodeInvalidParams   Code = 2001
	CodeUserExist       Code = 2002
	CodeUserNotExist    Code = 2003
	CodeInvalidPassword Code = 2004
	CodeInvalidToken    Code = 2006
	CodeInvalidCaptcha  Code = 2008

	CodeServerBusy Code = 4001
	CodeRateLimited Code = 4002

	AIModelFail Code = 5003
)

var msg = map[Code]string{
	CodeSuccess: "success",

	CodeInvalidParams:   "请求参数错误",
	CodeUserExist:       "该邮箱已经注册过账号",
	CodeUserNotExist:    "用户不存在",
	CodeInvalidPassword: "用户名或密码错误",
	CodeInvalidToken:    "无效的Token",
	CodeInvalidCaptcha:  "验证码错误",

	CodeServerBusy:  "服务繁忙",
	CodeRateLimited: "请求过于频繁，请稍后再试",

	AIModelFail: "模型运行失败",
}

func (code Code) Code() int64 {
	return int64(code)
}

// Msg 获取响应消息
func (code Code) Msg() string {
	if m, ok := msg[code]; ok {
		return m
	}
	return msg[CodeServerBusy]
}
