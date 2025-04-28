package handlers

import (
	"fmt"

	"github.com/bwmarrin/discordgo"
	"github.com/spf13/viper"
	"go.uber.org/zap"
)

func InitDiscordSession(logger *zap.Logger) *discordgo.Session {
	discord, err := discordgo.New("Bot " + viper.GetString("discord.token"))
	if err != nil {
		logger.Fatal("Discordセッションの作成に失敗しました", zap.Error(err))
	}
	return discord
}

func CreateMessageEmbed(data map[string]interface{}) *discordgo.MessageEmbed {
	return &discordgo.MessageEmbed{
		Title:       fmt.Sprintf("%s 分析結果", data["site"]),
		Description: "AIによる市場分析",
		Color:       0x0099ff,
		Fields:      []*discordgo.MessageEmbedField{},
	}
}
