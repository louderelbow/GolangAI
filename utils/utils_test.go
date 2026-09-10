package utils

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"deeptalk/config"
	"deeptalk/model"

	"github.com/cloudwego/eino/schema"
)

func setupConfig(t *testing.T) {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	root := filepath.Dir(filepath.Dir(file)) // utils/xxx_test.go -> 模块根目录
	example := filepath.Join(root, "config", "config.toml.example")
	if _, err := os.Stat(example); err != nil {
		t.Fatalf("找不到示例配置: %v", err)
	}
	t.Setenv("DEEPTALK_CONFIG", example)
}
// ConvertToSchemaMessages 必须把"易变的当前时间"放在稳定前缀之后，
// 否则每次都改写前缀，上游 prompt 前缀缓存永远命中不了。
func TestConvertToSchemaMessagesKeepsStablePrefix(t *testing.T) {
	setupConfig(t)
	cfg := config.GetConfig()

	msgs := []*model.Message{
		{Content: "第一轮问题", IsUser: true},
		{Content: "第一轮回答", IsUser: false},
		{Content: "第二轮问题", IsUser: true},
	}
	out := ConvertToSchemaMessages(msgs, "更早的摘要")

	if len(out) == 0 {
		t.Fatal("empty messages")
	}

	// 第一条必须是配置里的固定 system 提示（稳定前缀的开头）
	if cfg.AiPromptConfig.SystemPrompt != "" {
		if out[0].Role != schema.System || out[0].Content != cfg.AiPromptConfig.SystemPrompt {
			t.Errorf("第 0 条应为固定 system 提示，实际 role=%v content=%.20s", out[0].Role, out[0].Content)
		}
	}

	// 含"当前时间"的 system 消息不能在开头
	if strings.Contains(out[0].Content, "当前时间") {
		t.Error("当前时间不能出现在第一条（会破坏前缀缓存）")
	}

	// 最后一条是历史消息（本轮提问），之前一条是"当前时间"
	last := out[len(out)-1]
	if last.Content != "第二轮问题" || last.Role != schema.User {
		t.Errorf("最后一条应为本轮提问，实际 role=%v content=%s", last.Role, last.Content)
	}
	beforeLast := out[len(out)-2]
	if beforeLast.Role != schema.System || !strings.Contains(beforeLast.Content, "当前时间") {
		t.Errorf("倒数第二条应为当前时间 system 消息，实际 role=%v content=%.20s", beforeLast.Role, beforeLast.Content)
	}

	// 历史顺序保持：摘要 -> 第一轮问题 -> 第一轮回答
	var userTurns []string
	for _, m := range out {
		if m.Role == schema.User {
			userTurns = append(userTurns, m.Content)
		}
	}
	if len(userTurns) != 2 || userTurns[0] != "第一轮问题" || userTurns[1] != "第二轮问题" {
		t.Errorf("用户消息顺序异常: %v", userTurns)
	}
}

// 没有历史时也要能正常组装（首轮提问）
func TestConvertToSchemaMessagesSingleTurn(t *testing.T) {
	setupConfig(t)
	config.GetConfig()

	out := ConvertToSchemaMessages([]*model.Message{{Content: "你好", IsUser: true}}, "")
	if len(out) == 0 {
		t.Fatal("empty output")
	}
	if out[len(out)-1].Content != "你好" {
		t.Errorf("最后一条应为提问，实际 %s", out[len(out)-1].Content)
	}
}
