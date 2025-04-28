package services

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"go.uber.org/zap"
	"bot/config"
	"gorm.io/gorm"
)

type SummaryService struct {
	cfg    *config.AIConfig
	logger *zap.Logger
	client *http.Client
	db     *gorm.DB
}

type Article struct {
	gorm.Model
	Site        string
	Title       string    
	URL         string    
	Hash        string    
	Content     string    
	Body        string    
	Summary     string    
	Category    string    
	PublishedAt time.Time
}

type DeepseekRequest struct {
	Model       string        `json:"model"`
	Messages    []Message     `json:"messages"`
	Temperature float64       `json:"temperature"`
	MaxTokens   int           `json:"max_tokens"`
}

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type DeepseekResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

func NewSummaryService(cfg *config.AIConfig, logger *zap.Logger, db *gorm.DB) *SummaryService {
	return &SummaryService{
		cfg:    cfg,
		logger: logger,
		db:     db,
		client: &http.Client{
			Timeout: time.Duration(cfg.Timeout) * time.Millisecond,
		},
	}
}

func (s *SummaryService) GenerateSummary(ctx context.Context, content string) (string, error) {
	prompt := fmt.Sprintf(`あなたは上場企業の決算ニュース要約アシスタントです。  
これから、過去6時間に収集されたニュース記事をまとめレポートを作成します。  

1. **記事単位の要約**  
   各記事について、Body を読んで 2～3 文（日本語200文字以内）で要点をまとめ、Summary フィールドに収まる形で出力してください。  
   - 売上高、経常利益、増配・減配、最高益・赤字転落など“数字”と“変化”を必ず含めること。  
   - カテゴリごとの違い（「決算」なら業績全体、「修正」なら修正前後の差分）を意識すること。

2. **6時間ダイジェスト**  
   全記事の要約を踏まえ、最後に「6時間のまとめ」として、注目すべきトレンド、関心度が高いテーマ、緊急度の高いニュースを3～5行でレポートしてください


【記事本文】
%s`, content)

	requestBody := DeepseekRequest{
		Model: s.cfg.Model,
		Messages: []Message{
			{
				Role:    "user",
				Content: prompt,
			},
		},
		Temperature: 0.7,
		MaxTokens:   500,
	}

	jsonBody, err := json.Marshal(requestBody)
	if err != nil {
		return "", fmt.Errorf("リクエストのマーシャリングに失敗しました: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", s.cfg.Endpoint, bytes.NewBuffer(jsonBody))
	if err != nil {
		return "", fmt.Errorf("リクエストの作成に失敗しました: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.cfg.APIKey)

	resp, err := s.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("APIリクエストに失敗しました: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("APIがエラーステータスを返しました: %s", resp.Status)
	}

	var response DeepseekResponse
	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		return "", fmt.Errorf("レスポンスの解析に失敗しました: %w", err)
	}

	if len(response.Choices) == 0 {
		return "", fmt.Errorf("有効な要約が生成されませんでした")
	}

	return response.Choices[0].Message.Content, nil
}

func (s *SummaryService) GenerateAndStoreSummary(ctx context.Context, articleID int, content string) error {
	summary, err := s.GenerateSummary(ctx, content)
	if err != nil {
		s.logger.Error("要約生成に失敗しました",
			zap.Int("article_id", articleID),
			zap.Error(err))
		return fmt.Errorf("要約生成に失敗しました: %w", err)
	}

	if err := s.db.WithContext(ctx).Model(&Article{}).
		Where("id = ?", articleID).
		Update("summary", summary).Error; err != nil {
		s.logger.Error("要約の保存に失敗しました",
			zap.Int("article_id", articleID),
			zap.Error(err))
		return fmt.Errorf("要約の保存に失敗しました: %w", err)
	}
	
	s.logger.Info("要約の生成と保存が完了しました",
		zap.Int("article_id", articleID),
		zap.String("summary", summary))

	return nil
}
