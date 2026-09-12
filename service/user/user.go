package user

import (
	"deeptalk/common/code"
	myemail "deeptalk/common/email"
	myredis "deeptalk/common/redis"
	"deeptalk/dao/user"
	"deeptalk/model"
	"deeptalk/utils"
	"deeptalk/utils/myjwt"
	"log"
	"time"
)

func Login(username, password string) (string, code.Code) {
	var userInformation *model.User
	var ok bool
	//1:判断用户是否存在
	if ok, userInformation = user.IsExistUser(username); !ok {

		return "", code.CodeUserNotExist
	}
	//2:判断用户是否密码账号正确
	if !utils.CheckPassword(userInformation.Password, password) {
		return "", code.CodeInvalidPassword
	}
	//3:返回一个Token
	token, err := myjwt.GenerateToken(userInformation.ID, userInformation.Username)

	if err != nil {
		return "", code.CodeServerBusy
	}
	return token, code.CodeSuccess
}

func Register(email, password, captcha string) (string, string, code.Code) {

	var ok bool
	var userInformation *model.User

	//1:先判断用户是否已经存在了
	if ok, _ := user.IsExistUserByEmail(email); ok {
		return "", "", code.CodeUserExist
	}

	//2:从redis中验证验证码是否有效
	if ok, _ := myredis.CheckCaptchaForEmail(email, captcha); !ok {
		return "", "", code.CodeInvalidCaptcha
	}

	//3：生成11位的账号
	username := utils.GetRandomNumbers(11)

	//4：注册到数据库中
	if userInformation, ok = user.Register(username, email, password); !ok {
		return "", "", code.CodeServerBusy
	}

	//5：将账号一并发送到对应邮箱上去，后续需要账号登录
	if err := myemail.SendCaptcha(email, username, user.UserNameMsg); err != nil {
		log.Printf("[Register] send account mail failed: email=%s username=%s err=%v", email, username, err)
	}

	// 6:生成Token
	token, err := myjwt.GenerateToken(userInformation.ID, userInformation.Username)

	if err != nil {
		return "", "", code.CodeServerBusy
	}

	return token, username, code.CodeSuccess
}

// 往指定邮箱发送验证码
func SendCaptcha(email_ string) code.Code {
	// 同一邮箱 60 秒内只允许发一次：否则该接口会被当成免费邮件发送器
	ok, err := myredis.TryAcquire(myredis.GenerateCaptchaCooldown(email_), time.Minute)
	if err != nil {
		log.Printf("[SendCaptcha] cooldown check failed: email=%s err=%v", email_, err)
		return code.CodeServerBusy
	}
	if !ok {
		return code.CodeRateLimited
	}

	send_code := utils.GetRandomNumbers(6)
	//1:先存放到redis
	if err := myredis.SetCaptchaForEmail(email_, send_code); err != nil {
		return code.CodeServerBusy
	}

	//2:再进行远程发送
	if err := myemail.SendCaptcha(email_, send_code, myemail.CodeMsg); err != nil {
		return code.CodeServerBusy
	}

	return code.CodeSuccess
}
