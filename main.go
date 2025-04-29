package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"net/url"
	"strings"
	"sync"
	"time"
	"bot/status"
	"github.com/glebarez/sqlite"
	"github.com/gocolly/colly/v2"
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
	

	var cfg config.Config
	if err := viper.Unmarshal(&cfg); err != nil {
		logger.Fatal("設定の読み込みに失敗しました", zap.Error(err))
	}
	summaryService := services.NewSummaryService(&cfg.AI, logger, db)
	scheduler := services.NewScheduler(discord, logger, summaryService)

	// フィルターパラメータは設定ファイルから取得可能
	kabutanFilter := viper.GetString("kabutan.filter") // 通常フィルター
	irFilter := viper.GetString("kabutan.ir_filter")   // IR専用フィルター

	// 通常モード（設定ファイルから間隔を取得）
	scheduler.AddTask(viper.GetString("scraping.interval"), func() {
		articles := scrapeKabutanArticles(logger, kabutanFilter)
		status.UpdatePlayingStatus(discord)
		if len(articles) > 0 {
			logger.Info("通常スクレイピング結果", zap.Int("記事数", len(articles)))
			processAndNotify(discord, logger, articles)
		}
	})

	// リアルタイムIR通知モード（市場時間中30秒間隔）
	// 6時間前からの記事を要約するタスク
	scheduler.AddTask("0 */6 * * *", func() {
		var articles []Article
		sixHoursAgo := time.Now().Add(-6 * time.Hour)

		if result := db.Where("published_at >= ? AND summary = ''", sixHoursAgo).Find(&articles); result.Error != nil {
			logger.Error("要約対象記事の取得に失敗しました", zap.Error(result.Error))
			return
		}

		logger.Info("要約処理を開始します",
			zap.Int("対象記事数", len(articles)),
			zap.Time("基準時刻", sixHoursAgo))

		for _, article := range articles {
			if err := summaryService.GenerateAndStoreSummary(context.Background(), int(article.ID), article.Body); err != nil {
				logger.Error("記事要約に失敗しました",
					zap.Int("article_id", int(article.ID)),
					zap.Error(err))
			}
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

	// 6時間ごとの本文再取得タスク
	scheduler.AddTask("0 */6 * * *", func() {
		var articles []Article
		twentyFourHoursAgo := time.Now().Add(-24 * time.Hour)

		// 最終スクレイピングから24時間以上経過かつリトライ回数3回未満の記事を取得
		if result := db.Where("last_scraped_at < ? AND retry_count < ?", twentyFourHoursAgo, 3).Find(&articles); result.Error != nil {
			logger.Error("再スクレイピング対象記事の取得に失敗しました", zap.Error(result.Error))
			return
		}

		logger.Info("本文の再スクレイピングを開始します",
			zap.Int("対象記事数", len(articles)),
			zap.Time("基準時刻", twentyFourHoursAgo))

		for _, article := range articles {
			newBody := getArticleBody(logger, article.URL)
			if newBody != "" && newBody != article.Body {
				// 本文が更新されている場合のみデータベースを更新
				updateResult := db.Model(&article).Updates(map[string]interface{}{
					"body":            newBody,
					"last_scraped_at": time.Now(),
					"retry_count":     gorm.Expr("retry_count + 1"),
				})

				if updateResult.Error != nil {
					logger.Error("本文更新に失敗しました",
						zap.Int("article_id", int(article.ID)),
						zap.Error(updateResult.Error))
				} else {
					logger.Info("本文を正常に更新しました",
						zap.Int("article_id", int(article.ID)),
						zap.Int("文字数", len(newBody)))
				}
			}
		}
	})
	scheduler.Start()
	// メインスレッドをブロック（ハートビート付き）
	logger.Info("メインスレッドを起動しました")
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		logger.Info("システムは動作中です",
			zap.Time("最終チェック", time.Now()),
		)
	}
}

// debug付き scrapeKabutanArticles 関数（ページネーション無効化）
func scrapeKabutanArticles(logger *zap.Logger, filterParam string) []map[string]interface{} {
	baseURL := "https://kabutan.jp/news/marketnews/"
	startURL := baseURL
	if filterParam != "" {
		startURL += "?" + filterParam
	}

	articles := make([]map[string]interface{}, 0)

	c := colly.NewCollector(
		colly.UserAgent("Mozilla/5.0"),
	)

	c.OnRequest(func(r *colly.Request) {
		logger.Info("訪問開始", zap.String("url", r.URL.String()))
	})

	c.OnHTML(".s_news_list.mgbt0 tr", func(e *colly.HTMLElement) {
		article := make(map[string]interface{})

		// 日時
		datetime := e.ChildAttr("td.news_time time", "datetime")
		if datetime != "" {
			article["date"] = datetime
		}

		// カテゴリ
		category := e.ChildText("td:nth-child(2) div.newslist_ctg")
		if category != "" {
			article["category"] = category
		}

	

		// 銘柄コード

		// タイトル + URL
		title := e.ChildText("td:nth-child(3) a")
		href := e.ChildAttr("td:nth-child(3) a", "href")
		if title != "" {
			article["title"] = title
		}
		if href != "" {
			u, _ := url.Parse(baseURL)
			article["url"] = u.ResolveReference(&url.URL{Path: href}).String()
		}

		// 必須項目チェック
		if !hasRequiredFields(article) {
			logger.Info("必須項目不足、スキップ", zap.Any("article", article))
			return
		}

		// URL正規化
		norm, err := normalizeURL(article["url"].(string))
		if err != nil {
			logger.Warn("URL正規化エラー", zap.String("url", article["url"].(string)), zap.Error(err))
			return
		}
		article["url"] = norm

		// 重複チェック
		hash := generateHash(article["title"].(string), article["url"].(string), norm)
		var exist Article
		if err := db.Where("url = ? OR hash = ?", norm, hash).First(&exist).Error; err == nil {
			logger.Info("すでに存在する記事、スキップ", zap.String("title", article["title"].(string)))
			return
		}

		// DB保存
		var pub time.Time
		if ds, ok := article["date"].(string); ok && ds != "" {
			pt, err := time.Parse(time.RFC3339, ds)
			if err != nil {
				logger.Warn("日時パースエラー", zap.String("date", ds), zap.Error(err))
				return
			}
			pub = pt
		}

		errMutex.Lock()
		defer errMutex.Unlock()

		if err := db.Create(&Article{
			Title:       article["title"].(string),
			URL:         norm,
			Hash:        hash,
			Content:     fmt.Sprintf("カテゴリ: %s", article["category"]),
			Category:    article["category"].(string),
			PublishedAt: pub,
		}).Error; err != nil {
			logger.Error("記事保存失敗", zap.String("title", article["title"].(string)), zap.Error(err))
		} else {
			logger.Info("記事保存成功", zap.String("title", article["title"].(string)))
			articles = append(articles, article)
		}
	})

	c.OnError(func(r *colly.Response, err error) {
		logger.Error("リクエストエラー", zap.String("url", r.Request.URL.String()), zap.Int("status", r.StatusCode), zap.Error(err))
	})

	err := c.Visit(startURL)
	if err != nil {
		logger.Error("サイト訪問エラー", zap.Error(err))
		return nil
	}

	return articles
}

// scrapeKabutanIR リアルタイムIR用スクレイパー
func scrapeKabutanIR(logger *zap.Logger, filterParam string) []map[string]interface{} {
	c := colly.NewCollector(
		colly.AllowedDomains("kabutan.jp"),
		colly.Async(true),
		colly.CacheDir("./.cache"),
	)
	logger.Info("set")

	c.Limit(&colly.LimitRule{
		DomainGlob:  "*",
		Parallelism: viper.GetInt("scraping.parallelism"),
		RandomDelay: time.Duration(viper.GetInt("scraping.delay_seconds")) * time.Second,
	})

	articles := make([]map[string]interface{}, 0)
	baseURL := "https://kabutan.jp/news/"

	c.OnHTML("#news_contents .s_news_list tr", func(e *colly.HTMLElement) {
		article := map[string]interface{}{
			"date":       e.ChildAttr("td.news_time time", "datetime"),
			"category":   e.ChildText("td:nth-child(2) div.newslist_ctg"),
			"is_urgent":  strings.Contains(e.ChildAttr("td:nth-child(2) div.newslist_ctg", "class"), "kk_b"),
			"stock_code": e.ChildAttr("td:nth-child(3)", "data-code"),
			"title":      e.ChildText("td:nth-child(4) a"),
			"url":        e.Request.AbsoluteURL(e.ChildAttr("td:nth-child(4) a", "href")),
		}

		if !hasRequiredFields(article) {
			logger.Warn("必須フィールド検証エラー（IR記事）", zap.Any("article", article))
			return
		}

		norm, err := normalizeURL(article["url"].(string))
		if err != nil {
			logger.Warn("IR記事URL正規化エラー", zap.Error(err), zap.String("original_url", article["url"].(string)))
			return
		}
		article["url"] = norm

		hash := generateHash(article["title"].(string), article["url"].(string), norm)
		var exist Article
		if err := db.Where("url = ? OR hash = ?", norm, hash).First(&exist).Error; err == nil {
			logger.Info("重複IR記事をスキップ", zap.String("title", article["title"].(string)), zap.String("hash", hash))
			return
		}

		errMutex.Lock()
		defer errMutex.Unlock()
		maxIRArticles := viper.GetInt("scraping.max_articles.ir")
		if len(articles) >= maxIRArticles {
			logger.Info("IR記事最大取得数に達したため処理を停止",
				zap.Int("max_articles", maxIRArticles))
			return
		}

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
			if len(articles) >= maxIRArticles {
				logger.Info("IR記事最大取得数に達したため処理を停止",
					zap.Int("max_articles", maxIRArticles))
				return
			}
		}
	})

	// ページネーション無効化（高頻度クローリングのため）
	// c.OnHTML(".pagination a[href]", func(e *colly.HTMLElement) {})

	startURL := baseURL
	if filterParam != "" {
		startURL += "?" + filterParam
	}
	c.Visit(startURL)
	c.Wait()

	return articles
}

func hasRequiredFields(article map[string]interface{}) bool {
	required := map[string]func(interface{}) bool{
		"date":     func(v interface{}) bool { _, ok := v.(string); return ok },
		"category": func(v interface{}) bool { _, ok := v.(string); return ok },
		"title":    func(v interface{}) bool { _, ok := v.(string); return ok },
		"url":      func(v interface{}) bool { _, ok := v.(string); return ok },
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
	decodedPath, err := url.PathUnescape(u.EscapedPath())
	if err != nil {
		return "", err
	}
	decodedPath = strings.ReplaceAll(decodedPath, "%3F", "?")
	u.Path = decodedPath

	if u.RawQuery != "" {
		return fmt.Sprintf("%s://%s%s?%s", u.Scheme, u.Host, u.Path, u.RawQuery), nil
	}
	return fmt.Sprintf("%s://%s%s", u.Scheme, u.Host, u.Path), nil
}

func truncateString(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..." // 切り詰め末尾に省略記号を追加
}

func getSummaryForDisplay(article map[string]interface{}) string {
	if summary, ok := article["summary"].(string); ok && summary != "" {
		return summary
	}
	return "要約生成中... しばらくお待ちください"
}

func getArticleBody(logger *zap.Logger, url string) string {
	c := colly.NewCollector(
		colly.Async(true),
		colly.MaxDepth(2),
	)

	c.Limit(&colly.LimitRule{
		DomainGlob:  "*",
		Parallelism: 3,
		RandomDelay: 1 * time.Second,
	})

	var body strings.Builder
	var wg sync.WaitGroup
	wg.Add(1)

	c.OnHTML("#news_body", func(e *colly.HTMLElement) {
		defer wg.Done()
		content := strings.TrimSpace(e.Text)
		if content == "" {
			logger.Warn("本文コンテンツが空です", zap.String("url", url))
			return
		}
		body.WriteString(content)
	})

	c.OnError(func(r *colly.Response, err error) {
		defer wg.Done()
		logger.Error("記事本文の取得に失敗しました",
			zap.String("url", url),
			zap.Int("status_code", r.StatusCode),
			zap.Error(err))
	})

	if err := c.Visit(url); err != nil {
		logger.Error("リクエスト開始に失敗しました",
			zap.String("url", url),
			zap.Error(err))
		return ""
	}

	wg.Wait()
	return body.String()
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

		embed := &discordgo.MessageEmbed{
			Author: &discordgo.MessageEmbedAuthor{
					Name:    "速報",
					IconURL: "https://kabutan.jp/favicon.ico",
			},
			Title:       article["title"].(string),
			URL:         article["url"].(string),
			Description: fmt.Sprintf("**カテゴリ**: %s\n", article["category"].(string)),  // ←ここで閉じる
			Fields: []*discordgo.MessageEmbedField{                                        // ←そしてカンマ
					{
							Name:   "公開日時",
							Value:  article["date"].(string),
							Inline: true,
					},
					// 必要なら他のフィールドも
			},
			Color:     color,
			Timestamp: article["date"].(string),
			Footer:    &discordgo.MessageEmbedFooter{Text: "Powered by Kabutan Scraper ver1.1.0"},
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
		// 型アサーションの前に存在チェックを追加
		urgent, _ := article["is_urgent"].(bool)
		if !urgent {
			continue
		}

		// フィールドの存在チェックを追加
		var (
			title, url, stockCode, category, body, date string
			ok                                          bool
		)

		if title, ok = article["title"].(string); !ok {
			logger.Error("記事タイトルが不正な形式です", zap.Any("article", article))
			continue
		}
		if url, ok = article["url"].(string); !ok {
			logger.Error("記事URLが不正な形式です", zap.Any("article", article))
			continue
		}
		if stockCode, ok = article["stock_code"].(string); !ok {
			logger.Error("銘柄コードが不正な形式です", zap.Any("article", article))
			continue
		}
		if category, ok = article["category"].(string); !ok {
			logger.Error("カテゴリが不正な形式です", zap.Any("article", article))
			continue
		}
		if body, ok = article["body"].(string); !ok {
			body = "本文がありません"
		}
		if date, ok = article["date"].(string); !ok {
			date = "日時不明"
		}

		embed := &discordgo.MessageEmbed{
			Author: &discordgo.MessageEmbedAuthor{
				Name:    "🚨 緊急IR通知 🚨",
				IconURL: "https://kabutan.jp/favicon.ico",
			},
			Title: title,
			URL:   url,
			Description: fmt.Sprintf("**銘柄コード**: %s\n**カテゴリ**: %s\n**本文**:\n%s",
				stockCode,
				category,
				truncateString(body, 1000)),
			Fields: []*discordgo.MessageEmbedField{
				{Name: "発表時刻", Value: date, Inline: true},
			},
			Color:     0xFF0000,
			Timestamp: date,
			Footer:    &discordgo.MessageEmbedFooter{Text: "ver1.0.2"},
		}
		if _, err := discord.ChannelMessageSendEmbed(channelID, embed); err != nil {
			logger.Error("緊急通知に失敗",
				zap.Error(err),
				zap.String("title", title),
				zap.String("url", url))
		}
	}
}

type Article struct {
	ID            uint   `gorm:"primaryKey"`
	Site          string `gorm:"index"`
	Title         string
	URL           string `gorm:"uniqueIndex;size:500"`
	Hash          string `gorm:"uniqueIndex;size:64"`
	Content       string
	Body          string `gorm:"type:text"`
	Summary       string `gorm:"type:text"`
	Category      string
	PublishedAt   time.Time
	CreatedAt     time.Time
	UpdatedAt     time.Time
	LastScrapedAt time.Time // 最終スクレイピング日時を追跡
	RetryCount    int       // リトライ回数を追跡
}
