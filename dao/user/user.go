package user

import (
	"context"
	"deeptalk/common/mysql"
	"deeptalk/model"
	"deeptalk/utils"

	"gorm.io/gorm"
)

const (
	CodeMsg     = "DeepTalk验证码如下(验证码仅限于2分钟有效): "
	UserNameMsg = "DeepTalk的账号如下，请保留好，后续可以用账号进行登录 "
)

var ctx = context.Background()

// 这边只能通过账号进行登录
func IsExistUser(username string) (bool, *model.User) {

	user, err := mysql.GetUserByUsername(username)

	if err == gorm.ErrRecordNotFound || user == nil {
		return false, nil
	}

	return true, user
}

func IsExistUserByEmail(email string) (bool, *model.User) {

	user, err := mysql.GetUserByEmail(email)

	if err == gorm.ErrRecordNotFound || user == nil {
		return false, nil
	}

	return true, user
}

func Register(username, email, password string) (*model.User, bool) {
	hashedPassword, err := utils.HashPassword(password)
	if err != nil {
		return nil, false
	}
	if user, err := mysql.InsertUser(&model.User{
		Email:    email,
		Name:     username,
		Username: username,
		Password: hashedPassword,
	}); err != nil {
		return nil, false
	} else {
		return user, true
	}
}
