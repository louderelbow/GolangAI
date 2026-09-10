package utils

import (
	"crypto/rand"
	"deeptalk/config"
	"deeptalk/model"
	"fmt"
	"math/big"
	"mime/multipart"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
)

// GetRandomNumbers 生成 num 位数字串（验证码/账号）
// 必须使用 crypto/rand：math/rand 的序列在知道种子/输出后可被预测，
// 用在验证码上等于给暴力破解开绿灯。
func GetRandomNumbers(num int) string {
	if num <= 0 {
		return ""
	}
	var sb strings.Builder
	sb.Grow(num)
	max := big.NewInt(10)
	for i := 0; i < num; i++ {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			// 极端情况下（系统熵源不可用）回退，保证不 panic
			sb.WriteByte(byte('0' + time.Now().UnixNano()%10))
			continue
		}
		sb.WriteByte(byte('0' + n.Int64()))
	}
	return sb.String()
}

func HashPassword(password string) (string, error) {
	bytes, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(bytes), nil
}

func CheckPassword(hashedPassword, password string) bool {
	err := bcrypt.CompareHashAndPassword([]byte(hashedPassword), []byte(password))
	return err == nil
}

func GenerateUUID() string {
	return uuid.New().String()
}

// 将 schema 消息转换为数据库可存储的格式
func ConvertToModelMessage(sessionID string, userName string, msg *schema.Message) *model.Message {
	return &model.Message{
		SessionID: sessionID,
		UserName:  userName,
		Content:   msg.Content,
	}
}

// 将数据库消息转换为 schema 消息（供 AI 使用）
//
// 顺序刻意设计成「稳定前缀在前、易变内容在后」，用来命中上游的 prompt 前缀缓存：
//   1. 固定 system 提示（配置项，永远不变）
//   2. 历史摘要（相对稳定）
//   3. 历史消息（前缀逐轮增长）
//   4. 当前时间（每次都变，必须放到后面，否则整个前缀每次都不同）
//   5. 本轮提问
//
// 以前把「当前时间」放在第 0 位，等于每次请求都把前缀改掉，缓存命中率恒为 0。
func ConvertToSchemaMessages(msgs []*model.Message, summary string) []*schema.Message {
	cfg := config.GetConfig()

	schemaMsgs := make([]*schema.Message, 0, len(msgs)+3)
	if cfg.AiPromptConfig.SystemPrompt != "" {
		schemaMsgs = append(schemaMsgs, &schema.Message{
			Role:    schema.System,
			Content: cfg.AiPromptConfig.SystemPrompt,
		})
	}
	if summary != "" {
		schemaMsgs = append(schemaMsgs, &schema.Message{
			Role:    schema.System,
			Content: "以下是本次会话更早之前内容的摘要，请把它当作已经发生过的上下文：\n" + summary,
		})
	}

	// 最后一条通常是本轮提问，单独放到末尾
	historyEnd := len(msgs)
	if historyEnd > 0 {
		historyEnd--
	}
	for i := 0; i < historyEnd; i++ {
		schemaMsgs = append(schemaMsgs, toSchemaMessage(msgs[i]))
	}

	schemaMsgs = append(schemaMsgs, &schema.Message{
		Role:    schema.System,
		Content: time.Now().Format("当前时间：2006-01-02 15:04:05 Monday"),
	})

	if len(msgs) > 0 {
		schemaMsgs = append(schemaMsgs, toSchemaMessage(msgs[len(msgs)-1]))
	}

	return schemaMsgs
}

func toSchemaMessage(m *model.Message) *schema.Message {
	role := schema.Assistant
	if m.IsUser {
		role = schema.User
	}
	return &schema.Message{Role: role, Content: m.Content}
}

// RemoveAllFilesInDir 删除目录中的所有文件（不删除子目录）
func RemoveAllFilesInDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // 目录不存在就算了
		}
		return err
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			filePath := filepath.Join(dir, entry.Name())
			if err := os.Remove(filePath); err != nil {
				return err
			}
		}
	}
	return nil
}

// ValidateFile 校验文件是否为允许的文本文件（.md 或 .txt）
func ValidateFile(file *multipart.FileHeader) error {
	// 校验文件扩展名
	ext := strings.ToLower(filepath.Ext(file.Filename))
	if ext != ".md" && ext != ".txt" {
		return fmt.Errorf("文件类型不正确，只允许 .md 或 .txt 文件，当前扩展名: %s", ext)
	}

	return nil
}
