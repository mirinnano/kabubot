package config

import (
	"fmt"
	"github.com/spf13/viper"
	"go.uber.org/zap"
)

var logger *zap.Logger

func InitConfig() {
	viper.SetConfigName("config")
	viper.AddConfigPath("configs")
	if err := viper.ReadInConfig(); err != nil {
		GetLogger().Fatal("設定ファイルの読み込みに失敗しました", zap.Error(err))
	}
}

func ValidateConfig() error {
	required := []string{"discord.token", "scraping.kabutan_urls", "ai.api_key"}
	for _, key := range required {
		if !viper.IsSet(key) {
			return fmt.Errorf("必須設定が不足しています: %s", key)
		}
	}
	return nil
}

func GetLogger() *zap.Logger {
	logger, _ := zap.NewProduction()
	return logger
}
