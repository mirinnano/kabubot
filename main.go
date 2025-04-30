package main

import (
	"bot/command"
	"bot/config"
	"bot/handlers"
	"bot/services"
	"bot/status"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/gocolly/colly/v2"
	"github.com/spf13/viper"
	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/bwmarrin/discordgo"
)
const version = "v1.2.2"

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
	db.AutoMigrate(&Article{}, TradersArticle{})
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
		logger.Debug("Discordボットがオンラインです",
			zap.String("ユーザー名", r.User.Username),
			zap.String("ユーザーID", r.User.ID),
			zap.Float64("接続遅延(ms)", s.HeartbeatLatency().Seconds()*1000),
		)
	})
	if err := commands.RegisterAll(discord, logger); err != nil {
		logger.Fatal("スラッシュコマンド登録に失敗しました", zap.Error(err))
}
status.StartStatsCollector(logger)

	var cfg config.Config
	if err := viper.Unmarshal(&cfg); err != nil {
		logger.Fatal("設定の読み込みに失敗しました", zap.Error(err))
	}
	summaryService := services.NewSummaryService(&cfg.AI, logger, db)
	scheduler := services.NewScheduler(discord, logger, summaryService)
	
	// フィルターパラメータは設定ファイルから取得可能
	kabutanFilter := viper.GetString("kabutan.filter") // 通常フィルター
	irFilter := viper.GetString("kabutan.ir_filter")   // IR専用フィルター
	registerPagingHandler(discord, logger, db)
	// 通常モード（設定ファイルから間隔を取得）
	scheduler.AddTask(viper.GetString("scraping.interval"), func() {
		articles := scrapeKabutanArticles(logger, kabutanFilter)
	

		status.UpdatePlayingStatus(discord)
		if len(articles) > 0 {
			logger.Debug("通常スクレイピング結果", zap.Int("記事数", len(articles)))
			processAndNotify(discord, logger, articles)
		}
	})
	scheduler.AddTask("0 * * * *", func() {
		sendHourlyNewsEmbed(discord, logger, db, 1)
})

	// リアルタイムIR通知モード（市場時間中30秒間隔）
	scheduler.AddTask("*/1 * * * *", func() {
		articles := scrapeKabutanIR(logger, irFilter)
		
		if len(articles) > 0 {
			logger.Debug("リアルタイムIR検出", zap.Int("件数", len(articles)))
			// 緊急記事のみ別ルートで通知
			processUrgentNotifications(discord, logger, articles)
		}
	})

	scheduler.AddTask("*/2 * * * *", func() {
    arts, err := ScrapeTradersNews(logger,db,"")
    if err != nil {
        logger.Error("TradersNews スクレイピング失敗", zap.Error(err))
        return
    }
    processTradersNotify(discord, logger, arts)
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
func registerPagingHandler(discord *discordgo.Session, logger *zap.Logger, db *gorm.DB) {
	discord.AddHandler(func(s *discordgo.Session, i *discordgo.InteractionCreate) {
			data := i.MessageComponentData()
			if !strings.HasPrefix(data.CustomID, "hourly_prev:") &&
				 !strings.HasPrefix(data.CustomID, "hourly_next:") {
					return
			}

			parts := strings.Split(data.CustomID, ":")
			page, err := strconv.Atoi(parts[1])
			if err != nil {
					return
			}

			// Deferred Update 応答
			if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
					Type: discordgo.InteractionResponseDeferredMessageUpdate,
			}); err != nil {
					logger.Error("Deferred 応答エラー", zap.Error(err))
					return
			}

			// Embed と Components を生成
			embed, comps := buildHourlyEmbed(logger, db, page)
			if embed == nil {
					return
			}

			// ポインタに包む
			embeds := []*discordgo.MessageEmbed{embed}
			components := comps

			// メッセージを編集
			if _, err := s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
					Embeds:     &embeds,      // *[]*discordgo.MessageEmbed
					Components: &components,  // *[]discordgo.MessageComponent
			}); err != nil {
					logger.Error("ページング更新エラー", zap.Error(err))
			}
	})
}
// sendHourlyNewsEmbed は、1時間ニュースのEmbedを初回送信します。
// page 引数は必ず1を渡してください（初回は第1ページ）。
func sendHourlyNewsEmbed(s *discordgo.Session, logger *zap.Logger, db *gorm.DB, page int) {
	embed, comps := buildHourlyEmbed(logger, db, page)
	if embed == nil {
			return
	}
	channelID := viper.GetString("discord.alert_channel")
	if _, err := s.ChannelMessageSendComplex(channelID, &discordgo.MessageSend{
			Embed:      embed,
			Components: comps,
	}); err != nil {
			logger.Error("Hourly embed 送信失敗", zap.Error(err))
	}
}


type TradersArticle struct {
	ID          uint      `gorm:"primaryKey"`
	Title       string    `gorm:"size:500"`
	URL         string    `gorm:"uniqueIndex;size:500"`
	Hash        string    `gorm:"uniqueIndex;size:64"`
	Category    string    `gorm:"size:100"`
	PublishedAt time.Time
	CreatedAt   time.Time
}
var weekdayRE = regexp.MustCompile(`\(.+?\)`)
func ScrapeTradersNews(logger *zap.Logger, db *gorm.DB, filterParam string) ([]TradersArticle, error) {
	baseURL := "https://www.traders.co.jp/news/list/ALL/1"
	if filterParam != "" {
		baseURL += "?" + filterParam
	}
	const maxArticles = 10
	var newArticles []TradersArticle
	c := colly.NewCollector(colly.UserAgent("Mozilla/5.0"))

	c.OnRequest(func(r *colly.Request) {
		logger.Debug("訪問開始", zap.String("url", r.URL.String()))
	})

	c.OnHTML(".news_container", func(e *colly.HTMLElement) {
		// 日時パース: "2025/04/29(火) 18:13" → "2025/04/29 18:13"
		if len(newArticles) >= maxArticles {
			return
		}
		// 日時パース: "2025/04/29(火) 18:13"等 → "2025/04/29 18:13"
		// 生の日時文字列取得
		ts := e.ChildText(".timestamp")
		ts = weekdayRE.ReplaceAllString(ts, "")
		
		ts = strings.TrimSpace(ts)
// JST ロケーションを読み込む
loc, err := time.LoadLocation("Asia/Tokyo")
if err != nil {
    logger.Error("ロケーションロード失敗", zap.Error(err))
    loc = time.FixedZone("JST", 9*3600)  // フォールバック
}
		// パース処理
		layout := "2006/01/02 15:04"
		parsedTime, err := time.ParseInLocation(layout, ts, loc)
		if err != nil {
				logger.Warn("日時パースエラー", zap.String("raw", ts), zap.Error(err))
				return
		}

	

		// タイトル + URL
		title := e.ChildText(".news_headline a.news_link")
		href := e.ChildAttr(".news_headline a.news_link", "href")
		if title == "" || href == "" {
			logger.Debug("必須項目不足、スキップ", zap.String("title", title))
			return
		}

		fullURL := resolveURL("https://www.traders.co.jp", href)

		// ハッシュ生成
		hash := generateHashs(title, fullURL)

		// 重複チェック
		var exist TradersArticle
		if err := db.Where("url = ? OR hash = ?", fullURL, hash).First(&exist).Error; err == nil {
			logger.Debug("すでに存在する記事、スキップ", zap.String("title", title))
			return
		}


// 汎用ボタン生成ヘルパー

		// DB保存
		article := TradersArticle{
			Title:       title,
			URL:         fullURL,
			Hash:        hash,
			Category:    "トレーダーズ",
			PublishedAt: parsedTime,
		}
		if err := db.Create(&article).Error; err != nil {
			logger.Error("記事保存失敗", zap.String("title", title), zap.Error(err))
			return
		}

		newArticles = append(newArticles, article)
	})

	c.OnError(func(r *colly.Response, err error) {
		logger.Error("Traders news crawl error", zap.Int("status", r.StatusCode), zap.Error(err))
	})

	if err := c.Visit(baseURL); err != nil {
		return nil, fmt.Errorf("サイト訪問エラー: %w", err)
	}

	return newArticles, nil
}
func resolveURL(baseStr, path string) string {
	u, _ := url.Parse(baseStr)
	r, _ := url.Parse(path)
	return u.ResolveReference(r).String()
}
func generateHashs(title, fullURL string) string {
	h := sha256.New()
	h.Write([]byte(title))
	h.Write([]byte(fullURL))
	return hex.EncodeToString(h.Sum(nil))
}
func newLinkButton(label, url string) discordgo.MessageComponent {
	return discordgo.Button{
			Label:    label,
			Style:    discordgo.LinkButton,
			URL:      url,
			Emoji:    &discordgo.ComponentEmoji{Name: "🔗"},
			Disabled: false,
	}
}
const (
	hourlyItemsPerPage = 8 // １ページあたりの記事数
)

// ページ数計算
func totalPages(n, perPage int) int {
	pages := n / perPage
	if n%perPage != 0 {
			pages++
	}
	return pages
}

// Embed 中のタイトルを切り詰め
func truncate(s string, max int) string {
	if len(s) <= max {
			return s
	}
	return s[:max] + "…"
}

// main.go（または適切なファイル）に追加
func buildHourlyEmbed(logger *zap.Logger, db *gorm.DB, page int) (*discordgo.MessageEmbed, []discordgo.MessageComponent) {
	cutoff := time.Now().Add(-1 * time.Hour)

	// 直近1時間の記事をDBから取得
	var recent []Article
	if err := db.
			Where("published_at >= ?", cutoff).
			Order("published_at DESC").
			Find(&recent).Error; err != nil {
			logger.Error("DB取得失敗 (hourly)", zap.Error(err))
			return nil, nil
	}
	if len(recent) == 0 {
			return nil, nil
	}

	// ページ数計算
	total := (len(recent)+hourlyItemsPerPage-1) / hourlyItemsPerPage
	if page < 1 {
			page = 1
	} else if page > total {
			page = total
	}
	start := (page - 1) * hourlyItemsPerPage
	end := start + hourlyItemsPerPage
	if end > len(recent) {
			end = len(recent)
	}

	// Fields 作成
	fields := make([]*discordgo.MessageEmbedField, 0, end-start)
	for _, a := range recent[start:end] {
			t := a.PublishedAt.In(time.FixedZone("JST", 9*3600)).Format("15:04")
			title := a.Title
			if len(title) > 50 {
					title = title[:50] + "…"
			}
			fields = append(fields, &discordgo.MessageEmbedField{
					Name:   t,
					Value:  fmt.Sprintf("[%s](%s)", title, a.URL),
					Inline: false,
			})
	}

	embed := &discordgo.MessageEmbed{
			Author: &discordgo.MessageEmbedAuthor{
					Name:    "🕒 直近1時間のニュース",
					IconURL: "https://kabutan.jp/favicon.ico",
			},
			Description: fmt.Sprintf(
					"※ %s ～ %s の記事を表示 (Page %d/%d)",
					cutoff.Format("15:04"), time.Now().Format("15:04"), page, total,
			),
			Color:     0x00BFFF,
			Fields:    fields,
			Timestamp: time.Now().Format(time.RFC3339),
			Footer:    &discordgo.MessageEmbedFooter{Text: "Powered by Kabutan Scraper"},
	}

	// ボタン生成
	row := discordgo.ActionsRow{}
	if page > 1 {
			row.Components = append(row.Components, discordgo.Button{
					Label:    "◀️ Prev",
					Style:    discordgo.PrimaryButton,
					CustomID: fmt.Sprintf("hourly_prev:%d", page-1),
			})
	}
	if page < total {
			row.Components = append(row.Components, discordgo.Button{
					Label:    "Next ▶️",
					Style:    discordgo.PrimaryButton,
					CustomID: fmt.Sprintf("hourly_next:%d", page+1),
			})
	}

	return embed, []discordgo.MessageComponent{row}
}


func processTradersNotify(s *discordgo.Session, logger *zap.Logger, arts []TradersArticle) {
	channelID := viper.GetString("discord.alert_channel")
	var categoryColors = map[string]int{
    "決算":    0xFF4500,
    "決算修正": 0xFF6347,
    "市場速報": 0x00BFFF,
    "トレーダーズ": 0x0099FF,
}
	for _, art := range arts {
			color := categoryColors["トレーダーズ"]

			embed := &discordgo.MessageEmbed{
					Author: &discordgo.MessageEmbedAuthor{
							Name:    "📰 Traders ニュース",
							IconURL: "https://www.traders.co.jp/static/favicon.ico?m=1642666535",
					},
					Title:       art.Title,
					URL:         art.URL,
					Description: "最新トレーダーズニュースを配信します",
					Fields: []*discordgo.MessageEmbedField{
							{Name: "公開日時", Value: art.PublishedAt.Format(time.RFC3339), Inline: true},
					},
					Color:     color,
					Timestamp: art.PublishedAt.Format(time.RFC3339),
					Footer:    &discordgo.MessageEmbedFooter{Text: "Powered by Traders Scraper "+ version, IconURL: "	https://www.traders.co.jp/static/favicon.ico?m=1642666535"},
					Thumbnail: &discordgo.MessageEmbedThumbnail{URL: "https://www.traders.co.jp/static/favicon.ico?m=1642666535"},
			}

			components := []discordgo.MessageComponent{
					discordgo.ActionsRow{
							Components: []discordgo.MessageComponent{
									newLinkButton("記事へ", art.URL),
							},
					},
			}

			if _, err := s.ChannelMessageSendComplex(channelID, &discordgo.MessageSend{
					Embed:      embed,
					Components: components,
			}); err != nil {
					logger.Error("Traders通知失敗", zap.Error(err))
			}
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
			logger.Debug("記事保存成功", zap.String("title", article["title"].(string)))
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
			logger.Debug("重複IR記事をスキップ", zap.String("title", article["title"].(string)), zap.String("hash", hash))
			return
		}

		errMutex.Lock()
		defer errMutex.Unlock()
		maxIRArticles := viper.GetInt("scraping.max_articles.ir")
		if len(articles) >= maxIRArticles {
			logger.Debug("IR記事最大取得数に達したため処理を停止",
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
				logger.Debug("IR記事最大取得数に達したため処理を停止",
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


func generateHash(title, rawURL, normalizedURL string) string {
	h := sha256.New()
	h.Write([]byte(title))
	h.Write([]byte(rawURL))
	h.Write([]byte(normalizedURL))
	return hex.EncodeToString(h.Sum(nil))
}

func processAndNotify(s *discordgo.Session, logger *zap.Logger, data []map[string]interface{}) {
	channelID := viper.GetString("discord.alert_channel")
	var categoryColors = map[string]int{
    "決算":    0xFF4500,
    "決算修正": 0xFF6347,
    "市場速報": 0x00BFFF,
    "トレーダーズ": 0x0099FF,
}
	for _, art := range data {
			title := art["title"].(string)
			url := art["url"].(string)
			category := art["category"].(string)
			date := art["date"].(string)

			color := categoryColors[category]
			if color == 0 {
					color = 0x00FF00
			}

			embed := &discordgo.MessageEmbed{
					Author: &discordgo.MessageEmbedAuthor{
							Name:    fmt.Sprintf("📢 市場速報 - %s", category),
							IconURL: "https://kabutan.jp/favicon.ico",
					},
					Title:       title,
					URL:         url,
					Description: fmt.Sprintf("**カテゴリ**: %s", category),
					Fields: []*discordgo.MessageEmbedField{
							{Name: "公開日時", Value: date, Inline: true},
					},
					Color:     color,
					Timestamp: date,
					
					Footer:    &discordgo.MessageEmbedFooter{Text: "Powered by Kabutan Scraper "+ version, IconURL: "https://kabutan.jp/favicon.ico"},
					Thumbnail: &discordgo.MessageEmbedThumbnail{URL: "https://kabutan.jp/favicon.ico"},
			}

			components := []discordgo.MessageComponent{
					discordgo.ActionsRow{
							Components: []discordgo.MessageComponent{
									newLinkButton("続きを読む", url),
							},
					},
			}

			if _, err := s.ChannelMessageSendComplex(channelID, &discordgo.MessageSend{
					Embed:      embed,
					Components: components,
			}); err != nil {
					logger.Error("速報通知失敗", zap.Error(err))
			}
	}
}

func processUrgentNotifications(s *discordgo.Session, logger *zap.Logger, data []map[string]interface{}) {
	channelID := viper.GetString("discord.urgent_channel")
	if channelID == "" {
			channelID = viper.GetString("discord.alert_channel")
	}
	var categoryColors = map[string]int{
    "決算":    0xFF4500,
    "決算修正": 0xFF6347,
    "市場速報": 0x00BFFF,
    "トレーダーズ": 0x0099FF,
}
	for _, art := range data {
			urgent, _ := art["is_urgent"].(bool)
			if !urgent {
					continue
			}
			title, _ := art["title"].(string)
			url, _ := art["url"].(string)
			stockCode, _ := art["stock_code"].(string)
			category, _ := art["category"].(string)
			body, _ := art["body"].(string)
			date, _ := art["date"].(string)

			color := categoryColors[category]
			if color == 0 {
					color = 0xFF0000
			}

			embed := &discordgo.MessageEmbed{
					Author: &discordgo.MessageEmbedAuthor{
							Name:    fmt.Sprintf("🚨 速報 - %s", category),
							IconURL: "https://kabutan.jp/favicon.ico",
					},
					Title:       title,
					URL:         url,
					Description: truncateString(body, 512),
					Fields: []*discordgo.MessageEmbedField{
							{Name: "銘柄コード", Value: stockCode, Inline: true},
							{Name: "発表時刻", Value: date, Inline: true},
					},
					Color:     color,
					Timestamp: date,
					Image: &discordgo.MessageEmbedImage{  // ← ここで画像を設定
						URL: fmt.Sprintf(
								"https://funit.api.kabutan.jp/jp/chart?c=%s&a=1&s=1&m=1&v=%d",
								stockCode,  // あるいは art.ID や銘柄コードの変数
								time.Now().Unix(),
						),
					},
					Footer:    &discordgo.MessageEmbedFooter{Text: version, IconURL: "https://kabutan.jp/favicon.ico"},
					Thumbnail: &discordgo.MessageEmbedThumbnail{URL: "https://kabutan.jp/favicon.ico"},
			}

			components := []discordgo.MessageComponent{
					discordgo.ActionsRow{
							Components: []discordgo.MessageComponent{
									newLinkButton("記事を読む", url),
							},
					},
			}

			if _, err := s.ChannelMessageSendComplex(channelID, &discordgo.MessageSend{
					Content:    "",
					Embed:      embed,
					Components: components,
			}); err != nil {
					logger.Error("緊急通知送信失敗", zap.Error(err))
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
