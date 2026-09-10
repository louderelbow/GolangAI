package session

import (
	"deeptalk/common/mysql"
	"deeptalk/model"
)

// MaxSessionListSize 单次返回的会话列表上限（避免一次拉取过多数据）
const MaxSessionListSize = 200

func GetSessionsByUserName(UserName string) ([]model.Session, error) {
	var sessions []model.Session
	// 必须显式排序：不写 ORDER BY 时数据库返回顺序不保证，前端列表会"看起来随机"
	err := mysql.DB.Where("user_name = ?", UserName).
		Order("created_at desc").
		Limit(MaxSessionListSize).
		Find(&sessions).Error
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
