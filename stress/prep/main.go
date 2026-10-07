// prep 为压测预生成测试数据：测试账号 + JWT + 会话 + 知识库索引。
//
// 为什么要有这一步：
//  1. 直接压登录接口会先被"按 IP 限流"挡住（5 个令牌、每 5 秒补 1 个），根本压不到业务；
//     这里直接往库里写账号并用项目的 JWT 密钥签发 token，绕开登录链路。
//  2. 每个用户必须有自己的会话，否则压测会全部挤到同一个会话上，
//     被 AIHelper 的会话锁（a.turn）串行化，测出来的是排队延迟而不是吞吐。
//  3. RAG 问答需要 uploads/<用户名>/ 下有文件并且已经建好向量索引，
//     否则 NewRAGQuery 会直接报错、静默降级成"不检索的裸问"。
//
// 用法：
//
//	go run ./stress/prep -users 50 -out stress/users.json
//	go run ./stress/prep -users 50 -clean          # 先清理旧的测试数据再重建
//	go run ./stress/prep -users 50 -skipDoc        # 不建知识库（测的是降级路径）
//	go run ./stress/prep -users 50 -verify         # 建完顺手打一次真实请求验通
//
// 注意：建索引会调用 embedding 服务。如果你已经把 ragModelConfig.baseUrl 指向了
// mockllm，请保持指向 mock 再跑本工具——否则文档用真实模型向量化、查询用 mock 向量，
// 检索结果会毫无意义。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"deeptalk/internal/infra/config"
	"deeptalk/internal/infra/mysql"
	"deeptalk/internal/infra/redis"
	"deeptalk/internal/rag"
	"deeptalk/model"
	"deeptalk/utils"
	"deeptalk/utils/myjwt"

	"github.com/gin-gonic/gin"
)

type userRecord struct {
	Username   string   `json:"username"`
	Token      string   `json:"token"`
	SessionIDs []string `json:"sessionIds"`
}

type output struct {
	GeneratedAt string       `json:"generatedAt"`
	BaseURL     string       `json:"baseUrl"`
	APIPrefix   string       `json:"apiPrefix"`
	ModelType   string       `json:"modelType"`
	Users       []userRecord `json:"users"`
}

var sampleDoc = `# 员工手册（压测样例）

## 年假制度
正式员工入职满一年后享有带薪年假 10 天，满三年 15 天，满五年 20 天。
年假需要在当年内使用完毕，确实无法休完的，经部门负责人批准最多可结转 5 天至次年第一季度。
申请年假需提前 3 个工作日在系统内提交，由直属主管审批。

## 病假制度
员工患病需要请假的，需提供二级以上医院开具的病假证明。
病假期间工资按当地最低工资标准的 80% 发放，医疗期依照国家相关规定执行。
连续病假超过 5 个工作日的，需同步报备人力资源部。

## 报销流程
单笔报销金额在 500 元以内的，由直属主管审批后即可提交财务。
单笔金额超过 500 元但不超过 5000 元的，需部门负责人二次审批。
单笔金额超过 5000 元的，需分管副总审批。
报销单据需在费用发生后 30 日内提交，超期原则上不予受理。

## 试用期规定
试用期为 3 个月，试用期员工参照本手册执行，但不享有年假。
试用期考核不合格的，公司可依法解除劳动合同。
`

func main() {
	users := flag.Int("users", 50, "生成多少个测试用户")
	prefix := flag.String("prefix", "stress", "测试账号前缀")
	password := flag.String("password", "stress@123", "测试账号密码")
	sessionsPerUser := flag.Int("sessions", 1, "每个用户建几个会话")
	modelType := flag.String("modelType", "2", "会话绑定的模型类型（2 RAG / 6 Unified Agent）")
	out := flag.String("out", "stress/users.json", "生成的数据写到哪个文件")
	baseURL := flag.String("base", "http://127.0.0.1:9090", "被测服务的地址（写入 users.json）")
	skipDoc := flag.Bool("skipDoc", false, "不建知识库索引（用于测 RAG 降级路径）")
	docFile := flag.String("doc", "", "用这个文件作为知识库内容（默认用内置样例）")
	clean := flag.Bool("clean", false, "先清理同名测试账号及其会话/消息/索引，再重建")
	cleanOnly := flag.Bool("cleanOnly", false, "只清理测试账号然后退出（不重建）")
	verify := flag.Bool("verify", false, "生成后打一次真实请求验证链路是否通")
	verbose := flag.Bool("verbose", false, "打印 GORM 的 SQL 日志")
	flag.Parse()

	// 默认静音 GORM：InitMysql 在 gin debug 模式下会逐条打印 SQL，批量造数据时刷屏
	if !*verbose {
		gin.SetMode(gin.ReleaseMode)
	}

	conf := config.GetConfig()
	if err := mysql.InitMysql(); err != nil {
		log.Fatalf("[prep] 初始化 MySQL 失败: %v", err)
	}
	redis.Init()
	if redis.Rdb == nil {
		log.Fatalf("[prep] 初始化 Redis 失败（建索引需要 Redis Stack）")
	}

	docContent := sampleDoc
	if *docFile != "" {
		b, err := os.ReadFile(*docFile)
		if err != nil {
			log.Fatalf("[prep] 读取文档失败: %v", err)
		}
		docContent = string(b)
	}

	// 先清理：--clean 时把同名账号彻底删掉（含会话、消息、上传文件、向量索引）
	if *clean || *cleanOnly {
		for i := 1; i <= *users; i++ {
			username := fmt.Sprintf("%s_%d", *prefix, i)
			if err := cleanupUser(username); err != nil {
				log.Printf("[prep] 清理 %s 失败: %v", username, err)
			}
		}
		log.Printf("[prep] 已清理 %d 个测试账号", *users)
	}

	// -cleanOnly：清完就退出，不要顺手重建（收尾清理时用这个）
	if *cleanOnly {
		log.Printf("[prep] -cleanOnly 已生效，退出")
		return
	}

	records := make([]userRecord, 0, *users)
	for i := 1; i <= *users; i++ {
		username := fmt.Sprintf("%s_%d", *prefix, i)

		u, err := ensureUser(username, *password)
		if err != nil {
			log.Fatalf("[prep] 创建用户 %s 失败: %v", username, err)
		}

		token, err := myjwt.GenerateToken(u.ID, u.Username)
		if err != nil {
			log.Fatalf("[prep] 签发 token 失败: %v", err)
		}

		sessionIDs, err := ensureSessions(username, *sessionsPerUser, *modelType)
		if err != nil {
			log.Fatalf("[prep] 创建会话失败: %v", err)
		}

		if !*skipDoc {
			if _, err := ensureKnowledgeBase(username, docContent); err != nil {
				log.Printf("[prep] 用户 %s 建索引失败（该用户将走 RAG 降级路径）: %v", username, err)
			}
		}

		records = append(records, userRecord{Username: username, Token: token, SessionIDs: sessionIDs})
		if i%10 == 0 || i == *users {
			log.Printf("[prep] 已生成 %d/%d", i, *users)
		}
	}

	result := output{
		GeneratedAt: time.Now().Format(time.RFC3339),
		BaseURL:     *baseURL,
		APIPrefix:   "/api/v1/AI",
		ModelType:   *modelType,
		Users:       records,
	}
	if err := os.MkdirAll(filepath.Dir(*out), 0755); err != nil {
		log.Fatalf("[prep] 创建输出目录失败: %v", err)
	}
	b, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		log.Fatalf("[prep] 序列化失败: %v", err)
	}
	if err := os.WriteFile(*out, b, 0644); err != nil {
		log.Fatalf("[prep] 写文件失败: %v", err)
	}

	log.Printf("[prep] 完成：%d 个用户 -> %s", len(records), *out)
	log.Printf("[prep] 账号样例：%s / %s（密码 %s）", records[0].Username, "见 users.json 里的 token", *password)
	log.Printf("[prep] 会话模型类型 modelType=%s，embedding 维度=%d",
		*modelType, conf.RagModelConfig.RagDimension)

	if *verify {
		runVerify(*baseURL, records[0])
	}
}

// ensureUser 找到或创建测试账号
func ensureUser(username, password string) (*model.User, error) {
	var u model.User
	err := mysql.DB.Where("username = ?", username).First(&u).Error
	if err == nil {
		return &u, nil
	}

	hashed, err := utils.HashPassword(password)
	if err != nil {
		return nil, err
	}
	created, err := mysql.InsertUser(&model.User{
		Name:     username,
		Email:    username + "@stress.local",
		Username: username,
		Password: hashed,
	})
	if err != nil {
		return nil, err
	}
	return created, nil
}

// ensureSessions 保证用户至少有 n 个会话（模型类型由会话绑定决定，创建后不可改）
func ensureSessions(username string, n int, modelType string) ([]string, error) {
	var existing []model.Session
	if err := mysql.DB.Where("user_name = ?", username).Find(&existing).Error; err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(existing))
	for _, s := range existing {
		ids = append(ids, s.ID)
	}
	if len(ids) >= n {
		return ids[:n], nil
	}

	for i := len(ids); i < n; i++ {
		s := &model.Session{
			ID:        utils.GenerateUUID(),
			UserName:  username,
			Title:     fmt.Sprintf("压测会话 %d", i+1),
			ModelType: modelType,
		}
		if err := mysql.DB.Create(s).Error; err != nil {
			return nil, err
		}
		ids = append(ids, s.ID)
	}
	return ids, nil
}

// ensureKnowledgeBase 在 uploads/<username>/ 下放一个文档并建好向量索引。
// 完全复用上传接口的那套流程（分片 -> embedding -> 写 Redis），保证和线上一致。
func ensureKnowledgeBase(username, content string) (string, error) {
	userDir := filepath.Join("uploads", username)
	if err := os.MkdirAll(userDir, 0755); err != nil {
		return "", err
	}

	// 已经有文件就不重复建（索引也是幂等的，但没必要反复嵌入）
	entries, err := os.ReadDir(userDir)
	if err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				return e.Name(), nil
			}
		}
	}

	filename := "stress_doc_" + shortID() + ".md"
	filePath := filepath.Join(userDir, filename)
	if err := os.WriteFile(filePath, []byte(content), 0644); err != nil {
		return "", err
	}

	indexer, err := rag.NewRAGIndexer(filename)
	if err != nil {
		_ = os.Remove(filePath)
		return "", fmt.Errorf("创建索引器失败: %w", err)
	}
	if err := indexer.IndexFile(context.Background(), filePath); err != nil {
		_ = os.Remove(filePath)
		_ = rag.DeleteIndex(context.Background(), filename)
		return "", fmt.Errorf("建索引失败: %w", err)
	}
	return filename, nil
}

// cleanupUser 彻底删除一个测试账号及其派生数据
func cleanupUser(username string) error {
	// 1. 删除向量索引（要先拿到文件名）
	userDir := filepath.Join("uploads", username)
	if entries, err := os.ReadDir(userDir); err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				if err := rag.DeleteIndex(context.Background(), e.Name()); err != nil {
					log.Printf("[prep] 删除索引 %s 失败: %v", e.Name(), err)
				}
			}
		}
	}
	_ = os.RemoveAll(userDir)

	// 2. 删除会话与消息
	var sessions []model.Session
	if err := mysql.DB.Where("user_name = ?", username).Find(&sessions).Error; err == nil && len(sessions) > 0 {
		ids := make([]string, 0, len(sessions))
		for _, s := range sessions {
			ids = append(ids, s.ID)
		}
		_ = mysql.DB.Where("session_id IN ?", ids).Delete(&model.Message{}).Error
	}
	_ = mysql.DB.Where("user_name = ?", username).Delete(&model.Session{}).Error
	_ = mysql.DB.Where("user_name = ?", username).Delete(&model.Message{}).Error

	// 3. 删除用户本身（软删除会留垃圾，这里硬删）
	return mysql.DB.Unscoped().Where("username = ?", username).Delete(&model.User{}).Error
}

// runVerify 用生成的数据打一次真实请求，确认 token / 会话 / 后端都通了
func runVerify(baseURL string, rec userRecord) {
	if len(rec.SessionIDs) == 0 {
		log.Printf("[verify] 该用户没有会话，跳过")
		return
	}
	body := fmt.Sprintf(`{"question":"年假有几天？","sessionId":"%s"}`, rec.SessionIDs[0])

	req, err := newJSONRequest(baseURL+"/api/v1/AI/chat/send", body, rec.Token)
	if err != nil {
		log.Printf("[verify] 构造请求失败: %v", err)
		return
	}
	start := time.Now()
	resp, err := httpClient.Do(req)
	if err != nil {
		log.Printf("[verify] 请求失败（后端没起？）: %v", err)
		return
	}
	defer resp.Body.Close()

	var parsed struct {
		StatusCode int64  `json:"status_code"`
		StatusMsg  string `json:"status_msg"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&parsed)

	log.Printf("[verify] HTTP=%d status_code=%d msg=%q 耗时=%s",
		resp.StatusCode, parsed.StatusCode, parsed.StatusMsg, time.Since(start).Round(time.Millisecond))
	if parsed.StatusCode == 1000 {
		log.Printf("[verify] 链路正常，可以开始压测了")
	} else {
		log.Printf("[verify] 未返回成功码，请先排查（1000=成功，4002=被限流，4003=配额用尽，5003/5004=模型侧失败）")
	}
}

// shortID 取 UUID 的前 12 位（去掉短横线）作为文件名的一部分
func shortID() string {
	s := strings.ReplaceAll(utils.GenerateUUID(), "-", "")
	if len(s) > 12 {
		s = s[:12]
	}
	return s
}

var httpClient = &http.Client{Timeout: 120 * time.Second}

func newJSONRequest(url, body, token string) (*http.Request, error) {
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	return req, nil
}
