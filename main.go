package main

import (
	"deeptalk/common/aihelper"
	"deeptalk/common/mysql"
	"deeptalk/common/rabbitmq"
	"deeptalk/common/redis"
	"deeptalk/config"
	"deeptalk/dao/message"
	"deeptalk/dao/session"
	"deeptalk/router"
	"fmt"
	"log"
)

func StartServer(addr string, port int) error {
	r := router.InitRouter()
	//服务器静态资源路径映射关系，这里目前不需要
	// r.Static(config.GetConfig().HttpFilePath, config.GetConfig().MusicFilePath)
	return r.Run(fmt.Sprintf("%s:%d", addr, port))
}

// 从数据库加载消息并初始化 AIHelperManager
func readDataFromDB() error {
	manager := aihelper.GetGlobalManager()

	// 每个会话在创建时就绑定了模型类型，恢复内存时必须以数据库记录为准
	modelTypes := make(map[string]string)
	sessions, err := session.GetAllSessions()
	if err != nil {
		log.Printf("[readDataFromDB] GetAllSessions failed: %v", err)
	} else {
		for i := range sessions {
			s := &sessions[i]
			if s.ModelType == "" || !aihelper.IsValidModelType(s.ModelType) {
				log.Printf("[readDataFromDB] session=%s has no valid model_type=%q, fallback to %s",
					s.ID, s.ModelType, aihelper.DefaultModelType)
				modelTypes[s.ID] = aihelper.DefaultModelType
				continue
			}
			modelTypes[s.ID] = s.ModelType
		}
	}

	// 从数据库读取所有消息
	msgs, err := message.GetAllMessages()
	if err != nil {
		return err
	}
	// 遍历数据库消息
	for i := range msgs {
		m := &msgs[i]
		modelType := modelTypes[m.SessionID]
		if modelType == "" {
			modelType = aihelper.DefaultModelType
		}
		config := map[string]interface{}{
			"username": m.UserName,
		}

		helper, err := manager.GetOrCreateAIHelper(m.UserName, m.SessionID, modelType, config)
		if err != nil {
			log.Printf("[readDataFromDB] failed to create helper for user=%s session=%s modelType=%s: %v", m.UserName, m.SessionID, modelType, err)
			continue
		}
		helper.AddMessage(m.Content, m.UserName, m.IsUser, false)
	}

	log.Println("AIHelperManager init success ")
	return nil
}

func main() {
	conf := config.GetConfig()
	host := conf.MainConfig.Host
	port := conf.MainConfig.Port
	//初始化mysql
	if err := mysql.InitMysql(); err != nil {
		log.Println("InitMysql error , " + err.Error())
		return
	}
	//初始化AIHelperManager
	readDataFromDB()

	//初始化redis
	redis.Init()
	log.Println("redis init success  ")
	rabbitmq.InitRabbitMQ()
	log.Println("rabbitmq init success  ")

	err := StartServer(host, port) // 启动 HTTP 服务
	if err != nil {
		panic(err)
	}
}
