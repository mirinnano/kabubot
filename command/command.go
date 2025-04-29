package commands

import (
   "fmt"
		"time"
    "github.com/bwmarrin/discordgo"
    "go.uber.org/zap"
		
)
const version = "v1.2.0"
// RegisterAll registers all slash commands for the bot
func RegisterAll(s *discordgo.Session, logger *zap.Logger) error {
    commands := []*discordgo.ApplicationCommand{
        {
            Name:        "scrape",
            Description: "スクレイプ操作を管理します",
            Options: []*discordgo.ApplicationCommandOption{
                {
                    Type:        discordgo.ApplicationCommandOptionSubCommand,
                    Name:        "now",
                    Description: "即時に全サイトをスクレイプ実行",
                },
                {
                    Type:        discordgo.ApplicationCommandOptionSubCommand,
                    Name:        "status",
                    Description: "スクレイプのステータスを表示",
                },
                {
                    Type:        discordgo.ApplicationCommandOptionSubCommand,
                    Name:        "toggle",
                    Description: "指定サイトのスクレイプをON/OFF",
                    Options: []*discordgo.ApplicationCommandOption{
                        {
                            Type:        discordgo.ApplicationCommandOptionString,
                            Name:        "site",
                            Description: "kabutan, traders, ir のいずれか",
                            Required:    true,
                        },
                    },
                },
            },
        },
        {
            Name:        "set",
            Description: "フィルタや設定をリアルタイム変更",
            Options: []*discordgo.ApplicationCommandOption{
                {
                    Type:        discordgo.ApplicationCommandOptionSubCommand,
                    Name:        "filter",
                    Description: "サイトのフィルタパラメータを更新",
                    Options: []*discordgo.ApplicationCommandOption{
                        {Type: discordgo.ApplicationCommandOptionString, Name: "site", Description: "kabutan, traders, ir", Required: true},
                        {Type: discordgo.ApplicationCommandOptionString, Name: "param", Description: "新しいフィルタ文字列", Required: true},
                    },
                },
            },
        },
        {
            Name:        "summary",
            Description: "指定記事のAI要約を生成して返す",
            Options: []*discordgo.ApplicationCommandOption{
                {Type: discordgo.ApplicationCommandOptionString, Name: "url", Description: "要約対象のURL", Required: true},
            },
        },
        {
            Name:        "health",
            Description: "Botのヘルスチェック情報を表示",
        },
        {
            Name:        "config",
            Description: "現在のBot設定を表示",
            Options: []*discordgo.ApplicationCommandOption{
                {
                    Type:        discordgo.ApplicationCommandOptionSubCommand,
                    Name:        "show",
                    Description: "全設定値を一覧表示",
                },
            },
        },
        {
            Name:        "logs",
            Description: "ログレベルを変更します",
            Options: []*discordgo.ApplicationCommandOption{
                {
                    Type:        discordgo.ApplicationCommandOptionString,
                    Name:        "level",
                    Description: "debug, info, warn, error のいずれか",
                    Required:    true,
                },
            },
        },
        {
            Name:        "subscribe",
            Description: "キーワード通知を登録/解除",
            Options: []*discordgo.ApplicationCommandOption{
                {Type: discordgo.ApplicationCommandOptionString, Name: "keyword", Description: "通知するキーワード", Required: true},
                {Type: discordgo.ApplicationCommandOptionBoolean, Name: "enable", Description: "登録(true)か解除(false)", Required: true},
            },
        },
        {
            Name:        "archive",
            Description: "取得済記事を検索",
            Options: []*discordgo.ApplicationCommandOption{
                {
                    Type:        discordgo.ApplicationCommandOptionSubCommand,
                    Name:        "search",
                    Description: "キーワードで記事を検索",
                    Options: []*discordgo.ApplicationCommandOption{
                        {Type: discordgo.ApplicationCommandOptionString, Name: "query", Description: "検索キーワード", Required: true},
                    },
                },
            },
        },
        {
            Name:        "version",
            Description: "Botのバージョンとデプロイ日時を表示",
        },
        {
            Name:        "help",
            Description: "利用可能なコマンド一覧を表示",
        },
    }

    for _, cmd := range commands {
        if _, err := s.ApplicationCommandCreate(s.State.User.ID, "", cmd); err != nil {
            logger.Error("スラッシュコマンド登録失敗", zap.Error(err), zap.String("command", cmd.Name))
            return err
        }
    }
    return nil
}
func respond(s *discordgo.Session, i *discordgo.InteractionCreate, logger *zap.Logger, message string) {
	err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
					Content: message,
			},
	})
	if err != nil {
			logger.Error("インタラクション応答に失敗", zap.Error(err))
	}
}
// HandleInteraction routes slash commands to their handlers
func HandleInteraction(s *discordgo.Session, i *discordgo.InteractionCreate, logger *zap.Logger) {
    switch i.ApplicationCommandData().Name {
    case "scrape":
        handleScrape(s, i, logger)
    case "set":
        handleSet(s, i, logger)
    case "summary":
        handleSummary(s, i, logger)
    case "health":
        handleHealth(s, i, logger)
    case "config":
        handleConfig(s, i, logger)
    case "logs":
        handleLogs(s, i, logger)
    case "subscribe":
        handleSubscribe(s, i, logger)
    case "archive":
        handleArchive(s, i, logger)
    case "version":
        handleVersion(s, i, logger)
    case "help":
        handleHelp(s, i, logger)
    default:
        logger.Warn("不明なコマンド", zap.String("name", i.ApplicationCommandData().Name))
    }
}

// 下記に各コマンドのハンドラーを実装してください
func handleScrape(s *discordgo.Session, i *discordgo.InteractionCreate, logger *zap.Logger) {}
func handleSet(s *discordgo.Session, i *discordgo.InteractionCreate, logger *zap.Logger) {}
func handleSummary(s *discordgo.Session, i *discordgo.InteractionCreate, logger *zap.Logger) {}
func handleHealth(s *discordgo.Session, i *discordgo.InteractionCreate, logger *zap.Logger) {
	now := time.Now().Format("2006-01-02 15:04:05")
	message := fmt.Sprintf("🟢 Bot稼働中\n現在時刻: %s", now)
	respond(s, i, logger, message)
}

func handleConfig(s *discordgo.Session, i *discordgo.InteractionCreate, logger *zap.Logger) {}
func handleLogs(s *discordgo.Session, i *discordgo.InteractionCreate, logger *zap.Logger) {}
func handleSubscribe(s *discordgo.Session, i *discordgo.InteractionCreate, logger *zap.Logger) {}
func handleArchive(s *discordgo.Session, i *discordgo.InteractionCreate, logger *zap.Logger) {}
func handleVersion(s *discordgo.Session, i *discordgo.InteractionCreate, logger *zap.Logger) {
	message := fmt.Sprintf("🤖 Bot バージョン: %s", version)
	respond(s, i, logger, message)
}
func handleHelp(s *discordgo.Session, i *discordgo.InteractionCreate, logger *zap.Logger) {}
