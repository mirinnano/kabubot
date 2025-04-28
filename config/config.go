package config

import (
	"fmt"


	"github.com/spf13/viper"
	"go.uber.org/zap"
)

var logger *zap.Logger

type Config struct {
	Discord          DiscordConfig   `mapstructure:"discord"`
	Scraping         ScrapingConfig  `mapstructure:"scraping"`
	FinancialMetrics FinancialConfig `mapstructure:"financial_metrics"`
	AI               AIConfig        `mapstructure:"ai"`
	Screening        ScreeningConfig `mapstructure:"screening"`
}

type DiscordConfig struct {
	Token       string `yaml:"token"`
	AlertChannel string `yaml:"alert_channel"`
	LogChannel  string `yaml:"log_channel"`
}

type AIConfig struct {
	Provider    string `mapstructure:"provider"`
	APIKey      string `mapstructure:"api_key"`
	Endpoint    string `mapstructure:"endpoint"`
	Model       string `mapstructure:"model"`
	Timeout     int    `mapstructure:"timeout"`
}

type ScrapingConfig struct {
	Interval        string   `mapstructure:"interval"`
	SummaryInterval string   `mapstructure:"summary_interval"`
	UserAgent       string   `mapstructure:"user_agent"`
	Timeout         int      `mapstructure:"timeout"`
	KabutanURLs     []string `mapstructure:"kabutan_urls"`
	ArticleStorage  string   `mapstructure:"article_storage"`
	MaxArticles     struct {
		Regular int `mapstructure:"regular"`
		IR      int `mapstructure:"ir"`
	} `mapstructure:"max_articles"`
	MaxPages      int `mapstructure:"max_pages"`
	Parallelism   int `mapstructure:"parallelism"`
	DelaySeconds int `mapstructure:"delay_seconds"`
}

type FinancialConfig struct {
	Targets        []string  `mapstructure:"targets"`
	AlertThresholds struct {
		PER float64 `mapstructure:"PER"`
		PBR float64 `mapstructure:"PBR"`
	} `mapstructure:"alert_thresholds"`
}

type ScreeningConfig struct {
	Conditions struct {
		Financial []string `mapstructure:"financial"`
		Growth    []string `mapstructure:"growth"`
	} `mapstructure:"conditions"`
}

func InitConfig() {
	viper.SetConfigName("config")
	viper.AddConfigPath("configs")
	viper.AutomaticEnv()
	
	if err := viper.ReadInConfig(); err != nil {
		GetLogger().Fatal("設定ファイルの読み込みに失敗しました", zap.Error(err))
	}
}

func ValidateConfig() error {
	required := []string{
		"discord.token",
		"scraping.kabutan_urls",
		"ai.api_key",
		"scraping.max_articles.regular",
		"scraping.max_articles.ir",
	}

	for _, key := range required {
		if !viper.IsSet(key) {
			return fmt.Errorf("必須設定が不足しています: %s", key)
		}
	}

	if viper.GetInt("scraping.max_pages") < 1 || viper.GetInt("scraping.max_pages") > 5 {
		return fmt.Errorf("無効なmax_pages値: %d (1-5の範囲で設定してください)", viper.GetInt("scraping.max_pages"))
	}

	return nil
}

func GetLogger() *zap.Logger {
	if logger == nil {
		logger, _ = zap.NewProduction(
			zap.Fields(
				zap.String("version", "1.1.0"),
				zap.String("environment", viper.GetString("environment")),
			),
		)
	}
	return logger
}
