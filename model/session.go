package model

import (
	"time"

	"gorm.io/gorm"
)

type Session struct {
	ID        string         `gorm:"primaryKey;type:varchar(36)" json:"id"`
	UserName  string         `gorm:"index;not null" json:"username"`
	Title     string         `gorm:"type:varchar(100)" json:"title"`
	ModelType string         `gorm:"type:varchar(8);not null;default:'2'" json:"model_type"` // 会话绑定的模型类型，创建后不可更改
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	DeletedAt gorm.DeletedAt `gorm:"index" json:"-"`
}

type SessionInfo struct {
	SessionID string `json:"sessionId"`
	Title     string `json:"name"`
	ModelType string `json:"modelType"`
}
