package email

import (
	"deeptalk/config"
	"fmt"
	"log"
	"time"

	"gopkg.in/gomail.v2"
)

const (
	CodeMsg = "DeepTalk验证码如下(验证码仅限于2分钟有效): "

	// smtpTimeout SMTP 连接/发送超时：不设置的话网络异常时请求会一直挂着
	smtpTimeout = 10 * time.Second
)

func SendCaptcha(email, code, msg string) error {
	m := gomail.NewMessage()

	// 发件人
	m.SetHeader("From", config.GetConfig().EmailConfig.Email)
	// 收件人
	m.SetHeader("To", email)
	// 主题
	m.SetHeader("Subject", "来自DeepTalk的信息")
	// 正文内容（纯文本形式，也可以用 text/html）
	m.SetBody("text/plain", msg+" "+code)

	// 配置 SMTP 服务器和授权码,587：是 SMTP 的明文/STARTTLS 端口号
	d := gomail.NewDialer("smtp.qq.com", 587, config.GetConfig().EmailConfig.Email, config.GetConfig().EmailConfig.Authcode)

	// gomail.v2 的 Dialer 没有超时字段，底层 net.Dial 也没有超时，
	// SMTP 不可达时会让请求长时间挂住；这里用 goroutine + 超时兜底，
	// 保证接口能在 smtpTimeout 内返回（调用方拿到明确的失败）。
	errCh := make(chan error, 1)
	go func() {
		errCh <- d.DialAndSend(m)
	}()

	select {
	case err := <-errCh:
		if err != nil {
			log.Printf("[email] DialAndSend failed: to=%s err=%v", email, err)
			return err
		}
		log.Printf("[email] send mail success: to=%s", email)
		return nil
	case <-time.After(smtpTimeout):
		log.Printf("[email] DialAndSend timeout after %s: to=%s", smtpTimeout, email)
		return fmt.Errorf("smtp send timeout after %s", smtpTimeout)
	}
}
