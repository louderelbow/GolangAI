package config

import (
	"log"
	"os"

	"github.com/BurntSushi/toml"
)

type MainConfig struct {
	Port    int    `toml:"port"`
	AppName string `toml:"appName"`
	Host    string `toml:"host"`
}

type EmailConfig struct {
	Authcode string `toml:"authcode"`
	Email    string `toml:"email" `
}

type RedisConfig struct {
	RedisPort     int    `toml:"port"`
	RedisDb       int    `toml:"db"`
	RedisHost     string `toml:"host"`
	RedisPassword string `toml:"password"`
}

type MysqlConfig struct {
	MysqlPort         int    `toml:"port"`
	MysqlHost         string `toml:"host"`
	MysqlUser         string `toml:"user"`
	MysqlPassword     string `toml:"password"`
	MysqlDatabaseName string `toml:"databaseName"`
	MysqlCharset      string `toml:"charset"`
}

type JwtConfig struct {
	ExpireDuration int    `toml:"expire_duration"`
	Issuer         string `toml:"issuer"`
	Subject        string `toml:"subject"`
	Key            string `toml:"key"`
}

type Rabbitmq struct {
	RabbitmqPort     int    `toml:"port"`
	RabbitmqHost     string `toml:"host"`
	RabbitmqUsername string `toml:"username"`
	RabbitmqPassword string `toml:"password"`
	RabbitmqVhost    string `toml:"vhost"`
}

type RagModelConfig struct {
	RagEmbeddingModel string `toml:"embeddingModel"`
	RagChatModelName  string `toml:"chatModelName"`
	RagDocDir         string `toml:"docDir"`
	RagBaseUrl        string `toml:"baseUrl"`
	RagDimension      int    `toml:"dimension"`
	RagApiKey         string `toml:"apiKey"`
	MaxContextTokens  int    `toml:"maxContextTokens"`
}

type VoiceServiceConfig struct {
	VoiceServiceApiKey    string `toml:"voiceServiceApiKey"`
	VoiceServiceSecretKey string `toml:"voiceServiceSecretKey"`
}

type Config struct {
	EmailConfig        `toml:"emailConfig"`
	RedisConfig        `toml:"redisConfig"`
	MysqlConfig        `toml:"mysqlConfig"`
	JwtConfig          `toml:"jwtConfig"`
	MainConfig         `toml:"mainConfig"`
	Rabbitmq           `toml:"rabbitmqConfig"`
	RagModelConfig     `toml:"ragModelConfig"`
	VoiceServiceConfig `toml:"voiceServiceConfig"`
}

type RedisKeyConfig struct {
	CaptchaPrefix   string
	IndexName       string
	IndexNamePrefix string
}

var DefaultRedisKeyConfig = RedisKeyConfig{
	CaptchaPrefix:   "captcha:%s",
	IndexName:       "rag_docs:%s:idx",
	IndexNamePrefix: "rag_docs:%s:",
}

var config *Config

func InitConfig() error {
	configFile := "config/config.toml"
	if _, err := os.Stat(configFile); os.IsNotExist(err) {
		configFile = "config/config.toml.example"
		log.Println("[WARNING] config.toml not found, using config.toml.example (please fill in your real config)")
	}

	if _, err := toml.DecodeFile(configFile, config); err != nil {
		log.Fatal(err.Error())
		return err
	}
	return nil
}

func GetConfig() *Config {
	if config == nil {
		config = new(Config)
		_ = InitConfig()
	}
	return config
}
