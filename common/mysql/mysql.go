package mysql

import (
	applogger "deeptalk/common/logger"
	"deeptalk/config"
	"deeptalk/model"
	"fmt"
	stdlog "log"
	"os"
	"strings"
	"time"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

var DB *gorm.DB

func InitMysql() error {
	host := config.GetConfig().MysqlHost
	port := config.GetConfig().MysqlPort
	dbname := config.GetConfig().MysqlDatabaseName
	username := config.GetConfig().MysqlUser
	password := config.GetConfig().MysqlPassword
	charset := config.GetConfig().MysqlCharset

	//dsn := fmt.Sprintf("%s:%s@tcp(%s:%s)/%s?charset=%s&parseTime=true&loc=Local", username, password, host, port, dbname, charset)
	dsn := fmt.Sprintf("%s:%s@tcp(%s:%d)/%s?charset=%s&parseTime=true&loc=Local", username, password, host, port, dbname, charset)

	db, err := gorm.Open(mysql.New(mysql.Config{
		DSN:                       dsn,
		DefaultStringSize:         256,
		DisableDatetimePrecision:  true,
		DontSupportRenameIndex:    true,
		DontSupportRenameColumn:   true,
		SkipInitializeWithVersion: false,
	}), &gorm.Config{
		Logger: newGormLogger(),
	})
	if err != nil {
		return err
	}

	sqlDB, err := db.DB()
	if err != nil {
		return err
	}
	sqlDB.SetMaxIdleConns(10)
	sqlDB.SetMaxOpenConns(100)
	sqlDB.SetConnMaxLifetime(time.Hour)

	DB = db

	return migration()
}

func migration() error {
	return DB.AutoMigrate(
		new(model.User),
		new(model.Session),
		new(model.Message),
	)
}

// newGormLogger 构造 gorm 的日志器。
//
// 这里修的是压测发现的头号 CPU 开销：
// 原实现只要 gin 处于 debug 模式就把 gorm 设成 logger.Info，于是**每条 SQL 都同步写
// stdout**——一次 RAG 问答有 7~8 条 SQL，26 RPS 下每秒两百多行，20 秒的 CPU profile 里
// os.(*File).Write 占 25.9%，其中 93.9% 来自 gorm。
// 更隐蔽的是：gorm 用的是它自己 log.New(os.Stdout, ...) 建的 logger，
// 所以对标准库 log 调用 SetOutput（我们的异步写入器）根本管不到它，必须在这里单独接管。
//
// 现在：默认只记慢查询和错误（Warn），输出也走异步写入器；
// 需要看全部 SQL 时用环境变量 DEEPTALK_SQL_LOG=info 打开。
func newGormLogger() gormlogger.Interface {
	level := gormlogger.Warn
	switch strings.ToLower(os.Getenv("DEEPTALK_SQL_LOG")) {
	case "info":
		level = gormlogger.Info
	case "error":
		level = gormlogger.Error
	case "silent":
		level = gormlogger.Silent
	}

	// 关键：writer 走 logger.Output()（开了异步就是异步批量写），而不是 gorm 默认的 os.Stdout
	writer := stdlog.New(applogger.Output(), "", stdlog.LstdFlags)
	return gormlogger.New(writer, gormlogger.Config{
		SlowThreshold:             200 * time.Millisecond,
		LogLevel:                  level,
		IgnoreRecordNotFoundError: true, // 查不到记录是业务常态（如用户不存在），不该刷日志
		Colorful:                  false,
	})
}

func InsertUser(user *model.User) (*model.User, error) {
	err := DB.Create(&user).Error
	return user, err
}

func GetUserByUsername(username string) (*model.User, error) {
	user := new(model.User)
	err := DB.Where("username = ?", username).First(user).Error
	return user, err
}

func GetUserByEmail(email string) (*model.User, error) {
	user := new(model.User)
	err := DB.Where("email = ?", email).First(user).Error
	return user, err
}
