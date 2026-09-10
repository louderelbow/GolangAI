package session

import (
	"deeptalk/common/mysql"
	"deeptalk/model"
)

func GetSessionsByUserName(UserName string) ([]model.Session, error) {
	var sessions []model.Session
	err := mysql.DB.Where("user_name = ?", UserName).Find(&sessions).Error
	return sessions, err
}

// GetAllSessions 加载所有会话（启动时恢复内存中的会话模型绑定）
func GetAllSessions() ([]model.Session, error) {
	var sessions []model.Session
	err := mysql.DB.Find(&sessions).Error
	return sessions, err
}

// GetSessionByID 按会话 ID 查询（用于确定该会话绑定的模型）
func GetSessionByID(id string) (*model.Session, error) {
	var s model.Session
	if err := mysql.DB.Where("id = ?", id).First(&s).Error; err != nil {
		return nil, err
	}
	return &s, nil
}

func CreateSession(session *model.Session) (*model.Session, error) {
	err := mysql.DB.Create(session).Error
	return session, err
}
