package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/spf13/viper"
	"go.uber.org/zap"
	"gorm.io/gorm"

	"bot/config"
	"bot/handlers"
	"bot/services"

	"github.com/bwmarrin/discordgo"
)

var (
	// 各スクレイパーは filterParam を受け取るシグネチャに統一
	sites = map[string]func(*zap.Logger, string) []map[string]interface{}{
		"kabutan":    scrapeKabutanArticles,
		"kabutan_ir": scrapeKabutanIR,
	}
	errMutex sync.Mutex
	db       *gorm.DB
)

func initDB() {
	var err error
	db, err = gorm.Open(sqlite.Open("articles.db"), &gorm.Config{})
	if err != nil {
		log.Fatalf("データベース接続に失敗しました: %v", err)
	}

	// 自動マイグレーション
	db.AutoMigrate(&Article{})
}

func main() {
	config.InitConfig()
	if err := config.ValidateConfig(); err != nil {
		log.Fatalf("設定検証エラー: %v", err)
	}

	logger := config.GetLogger()
	defer logger.Sync()

	initDB()

	// Discordセッションの初期化と接続
	discord := handlers.InitDiscordSession(logger)
	if err := discord.Open(); err != nil {
		logger.Fatal("Discord接続に失敗しました",
			zap.Error(err),
			zap.String("トークン", viper.GetString("discord.token")),
			zap.String("設定ファイル", viper.ConfigFileUsed()),
		)
		defer discord.Close()
		return
	}

	discord.AddHandler(func(s *discordgo.Session, r *discordgo.Ready) {
		logger.Info("Discordボットがオンラインです",
			zap.String("ユーザー名", r.User.Username),
			zap.String("ユーザーID", r.User.ID),
			zap.Float64("接続遅延(ms)", s.HeartbeatLatency().Seconds()*1000),
		)
	})

	scheduler := services.NewScheduler(discord, logger)
	scheduler.Start()

	// フィルターパラメータは設定ファイルから取得可能
	kabutanFilter := viper.GetString("kabutan.filter")       // 通常フィルター
	irFilter := viper.GetString("kabutan.ir_filter")         // IR専用フィルター

	// 通常モード（5分間隔）
	scheduler.AddTask("*/5 * * * *", func() {
		articles := scrapeKabutanArticles(logger, kabutanFilter)
		if len(articles) > 0 {
			logger.Info("通常スクレイピング結果", zap.Int("記事数", len(articles)))
			processAndNotify(discord, logger, articles)
		}
	})

	// リアルタイムIR通知モード（市場時間中30秒間隔）
	scheduler.AddTask("*/1 * * * *", func() {
		articles := scrapeKabutanIR(logger, irFilter)
		if len(articles) > 0 {
			logger.Info("リアルタイムIR検出", zap.Int("件数", len(articles)))
			// 緊急記事のみ別ルートで通知
			processUrgentNotifications(discord, logger, articles)
		}
	})

	// メインスレッドをブロック（ハートビート付き）
	logger.Info("メインスレッドを起動しました")
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			logger.Info("システムは動作中です",
				zap.Time("最終チェック", time.Now()),
			)
		}
	}
}

// scrapeKabutanArticles 通常ニュース用スクレイパー
func scrapeKabutanArticles(logger *zap.Logger, filterParam string) []map[string]interface{} {
	baseURL := "https://kabutan.jp/news/marketnews/"
	// filterParam があればクエリ文字列として付与
	startURL := baseURL
	if filterParam != "" {
		startURL += "?" + filterParam
	}
	articles := make([]map[string]interface{}, 0)

	l := launcher.New().Headless(true)
	defer l.Cleanup()
	browser := rod.New().ControlURL(l.MustLaunch()).MustConnect()
	defer browser.MustClose()

	page := browser.MustPage(startURL)
	page.Timeout(30 * time.Second).MustWaitLoad().MustWaitStable()

	// テーブル要素取得 with retry
	var table *rod.Element
	for i := 0; i < 3; i++ {
		t, err := page.Element(".s_news_list.mgbt0")
		if err == nil {
			table = t
			break
		}
		logger.Warn("テーブル要素取得リトライ", zap.Int("attempt", i+1), zap.Error(err))
		time.Sleep(2 * time.Second)
	}
	if table == nil {
		logger.Error("テーブル要素の取得に失敗しました")
		return nil
	}
	rows := table.MustElements("tr")

	for _, row := range rows {
		article := make(map[string]interface{})

		// 日時
		if tEl, err := row.Element("td.news_time time"); err == nil {
			if dt, _ := tEl.Attribute("datetime"); dt != nil {
				article["date"] = *dt
			}
		}

		// カテゴリ + 緊急フラグ
		if cEl, err := row.Element("td:nth-child(2) div.newslist_ctg"); err == nil {
			article["category"] = cEl.MustText()
			if cl, _ := cEl.Attribute("class"); cl != nil {
				article["is_urgent"] = strings.Contains(*cl, "kk_b")
			}
		}

		// 銘柄コード
		if codeEl, err := row.Element("td:nth-child(3)"); err == nil {
			if cd, _ := codeEl.Attribute("data-code"); cd != nil {
				article["stock_code"] = *cd
			}
		}

		// タイトル + URL
		if titleEl, err := row.Element("td:nth-child(4) a"); err == nil {
			article["title"] = titleEl.MustText()
			if href, _ := titleEl.Attribute("href"); href != nil {
				u, _ := url.Parse(baseURL)
				article["url"] = u.ResolveReference(&url.URL{Path: *href}).String()
			}
		}

		if !hasRequiredFields(article) {
			continue
		}

		// URL 正規化 + 重複フィルタ
		norm, err := normalizeURL(article["url"].(string))
		if err != nil {
			logger.Warn("URL正規化エラー", zap.Error(err))
			continue
		}
		article["url"] = norm

		hash := generateHash(article["title"].(string), article["url"].(string), norm)
		var exist Article
		if err := db.Where("url = ? OR hash = ?", norm, hash).First(&exist).Error; err == nil {
			continue
		}

		// DB 保存
		var pub time.Time
		if ds, ok := article["date"].(string); ok && ds != "" {
			pt, err := time.Parse(time.RFC3339, ds)
			if err != nil {
				logger.Warn("日時パースエラー", zap.String("date", ds), zap.Error(err))
				continue
			}
			pub = pt
		}

		errMutex.Lock()
		if err := db.Create(&Article{
			Title:       article["title"].(string),
			URL:         norm,
			Hash:        hash,
			Content:     fmt.Sprintf("カテゴリ: %s", article["category"]),
			Category:    article["category"].(string),
			PublishedAt: pub,
		}).Error; err != nil {
			logger.Error("記事保存失敗", zap.Error(err))
		} else {
			articles = append(articles, article)
		}
		errMutex.Unlock()
	}

	page.MustClose()
	return articles
}

// scrapeKabutanIR リアルタイムIR用スクレイパー
func scrapeKabutanIR(logger *zap.Logger, filterParam string) []map[string]interface{} {
	base := "https://kabutan.jp/news/"
	startURL := base
	if filterParam != "" {
		startURL += "?" + filterParam
	}

	articles := make([]map[string]interface{}, 0)
	l := launcher.New().Headless(true)
	defer l.Cleanup()
	browser := rod.New().ControlURL(l.MustLaunch()).MustConnect()
	defer browser.MustClose()

	page := browser.MustPage(startURL)

	for {
		page.MustWaitLoad().MustWaitIdle()
		rows := page.MustElements("#news_contents .s_news_list tr")

		for _, row := range rows {
			article := make(map[string]interface{})
			if tEl, err := row.Element("td.news_time time"); err == nil {
				if dt, _ := tEl.Attribute("datetime"); dt != nil {
					article["date"] = *dt
				}
			}
			if cEl, err := row.Element("td:nth-child(2) div.newslist_ctg"); err == nil {
				article["category"] = cEl.MustText()
				if cl, _ := cEl.Attribute("class"); cl != nil {
					article["is_urgent"] = strings.Contains(*cl, "kk_b")
				}
			}
			if codeEl, err := row.Element("td:nth-child(3)"); err == nil {
				if cd, _ := codeEl.Attribute("data-code"); cd != nil {
					article["stock_code"] = *cd
				}
			}
			if titleEl, err := row.Element("td:nth-child(4) a"); err == nil {
				article["title"] = titleEl.MustText()
				if href, _ := titleEl.Attribute("href"); href != nil {
					u, _ := url.Parse(base)
					article["url"] = u.ResolveReference(&url.URL{Path: *href}).String()
				}
			}

			if !hasRequiredFields(article) {
				logger.Warn("必須フィールド検証エラー（IR記事）",
					zap.Any("article", article),
					zap.String("url", page.MustInfo().URL),
				)
				continue
			}

			// URL正規化と重複チェック
			norm, err := normalizeURL(article["url"].(string))
			if err != nil {
				logger.Warn("IR記事URL正規化エラー", 
					zap.Error(err),
					zap.String("original_url", article["url"].(string)),
				)
				continue
			}
			article["url"] = norm

			hash := generateHash(article["title"].(string), article["url"].(string), norm)
			var exist Article
			if err := db.Where("url = ? OR hash = ?", norm, hash).First(&exist).Error; err == nil {
				logger.Info("重複IR記事をスキップ",
					zap.String("title", article["title"].(string)),
					zap.String("hash", hash),
				)
				continue
			}

			// DB保存処理
			errMutex.Lock()
			if err := db.Create(&Article{
				Title:       article["title"].(string),
				URL:         norm,
				Hash:        hash,
				Content:     fmt.Sprintf("IRカテゴリ: %s", article["category"].(string)),
				Category:    article["category"].(string),
				PublishedAt: time.Now(),
			}).Error; err != nil {
				logger.Error("IR記事保存失敗", zap.Error(err))
			} else {
				articles = append(articles, article)
			}
			errMutex.Unlock()
		}

		// 次ページ処理（3回リトライ）
		var nextHref string
		for retry := 0; retry < 3; retry++ {
			nextLink, err := page.Element("a:contains('次へ＞')")
			if err != nil || nextLink == nil {
				break
			}
			
			if href, err := nextLink.Attribute("href"); err == nil && href != nil {
				nextHref = *href
				break
			}
			
			logger.Warn("次ページリンク取得リトライ", 
				zap.Int("attempt", retry+1),
				zap.String("url", page.MustInfo().URL),
			)
			time.Sleep(time.Duration(retry+1) * time.Second)
		}

		if nextHref == "" {
			break
		}

		// 正規化されたURLで新しいページを開く
		u, err := url.Parse(nextHref)
		if err != nil {
			logger.Error("URL解析エラー", 
				zap.String("href", nextHref),
				zap.Error(err),
			)
			break
		}
		page = browser.MustPage(u.String())
	}

	return articles
}

func hasRequiredFields(article map[string]interface{}) bool {
	required := map[string]func(interface{}) bool{
		"date":       func(v interface{}) bool { _, ok := v.(string); return ok },
		"category":   func(v interface{}) bool { _, ok := v.(string); return ok },
		"title":      func(v interface{}) bool { _, ok := v.(string); return ok },
		"url":        func(v interface{}) bool { _, ok := v.(string); return ok },
		"stock_code": func(v interface{}) bool { 
			s, ok := v.(string)
			return ok && len(s) == 4 && strings.Trim(s, "0123456789") == ""
		},
	}

	for key, validate := range required {
		val, exists := article[key]
		if !exists || !validate(val) {
			return false
		}
	}
	
	// 日付形式の検証
	if _, err := time.Parse(time.RFC3339, article["date"].(string)); err != nil {
		return false
	}

	return true
}

func normalizeURL(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	// パス部のデコードと特殊文字置換
	decodedPath, err := url.PathUnescape(u.EscapedPath())
	if err != nil {
		return "", err
	}
	// %3F → ? 置換
	decodedPath = strings.ReplaceAll(decodedPath, "%3F", "?")
	u.Path = decodedPath
	
	return fmt.Sprintf("%s://%s%s", u.Scheme, u.Host, u.Path), nil
}

func generateHash(title, rawURL, normalizedURL string) string {
	h := sha256.New()
	h.Write([]byte(title))
	h.Write([]byte(rawURL))
	h.Write([]byte(normalizedURL))
	return hex.EncodeToString(h.Sum(nil))
}

func processAndNotify(discord *discordgo.Session, logger *zap.Logger, data []map[string]interface{}) {
	channelID := viper.GetString("discord.alert_channel")
	for _, article := range data {
		// カラーマッピング
		color := 0x00FF00 // デフォルト緑
		switch article["category"].(string) {
		case "修正":
			color = 0xFFA500 // オレンジ
		case "決算":
			color = 0x0000FF // 青
		}

		embed := &discordgo.MessageEmbed{
			Author: &discordgo.MessageEmbedAuthor{
				Name:    "Kabutan 決算速報",
				URL:     "https://kabutan.jp/news/",
				IconURL: "https://kabutan.jp/favicon.ico",
			},
			Title:       article["title"].(string),
			URL:         article["url"].(string),
			Description: fmt.Sprintf("**カテゴリ**: %s", article["category"].(string)),
			Fields: []*discordgo.MessageEmbedField{
				{Name: "銘柄コード", Value: article["stock_code"].(string), Inline: true},
				{Name: "公開日時", Value: article["date"].(string), Inline: true},
			},
			Color:     color,
			Timestamp: article["date"].(string),
			Thumbnail: &discordgo.MessageEmbedThumbnail{URL: fmt.Sprintf("https://kabutan.jp/stock/?code=%s&image=logo", article["stock_code"].(string))},
			Footer: &discordgo.MessageEmbedFooter{Text: "Powered by Kabutan Scraper"},
		}
		if _, err := discord.ChannelMessageSendEmbed(channelID, embed); err != nil {
			logger.Error("Discord通知に失敗", zap.Error(err))
		}
	}
}

func processUrgentNotifications(discord *discordgo.Session, logger *zap.Logger, data []map[string]interface{}) {
	channelID := viper.GetString("discord.urgent_channel")
	if channelID == "" {
		channelID = viper.GetString("discord.alert_channel")
	}
	for _, article := range data {
		if urgent, ok := article["is_urgent"].(bool); !ok || !urgent {
			continue
		}
		embed := &discordgo.MessageEmbed{
			Author: &discordgo.MessageEmbedAuthor{
				Name:    "🚨 緊急IR通知 🚨",
				IconURL: "https://kabutan.jp/favicon.ico",
			},
			Title:       article["title"].(string),
			URL:         article["url"].(string),
			Description: fmt.Sprintf("**銘柄コード**: %s\n**カテゴリ**: %s", article["stock_code"], article["category"]),
			Fields: []*discordgo.MessageEmbedField{
				{Name: "発表時刻", Value: article["date"].(string), Inline: true},
			},
			Color:     0xFF0000,
			Timestamp: article["date"].(string),
			Footer:    &discordgo.MessageEmbedFooter{Text: "ver1.0.2"},
		}
		if _, err := discord.ChannelMessageSendEmbed(channelID, embed); err != nil {
			logger.Error("緊急通知に失敗", zap.Error(err))
		}
	}
}


type Article struct {
	ID        uint      `gorm:"primaryKey"`
	Site      string    `gorm:"index"`
	Title     string
	URL       string    `gorm:"uniqueIndex;size:500"`
	Hash      string    `gorm:"uniqueIndex;size:64"`
	Content   string
	Category  string
	PublishedAt time.Time
	CreatedAt time.Time
	UpdatedAt time.Time
}
