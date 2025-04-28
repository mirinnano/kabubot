package services

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/go-co-op/gocron"
	"go.uber.org/zap"
)

type Scheduler struct {
	*gocron.Scheduler
	logger         *zap.Logger
	summaryService *SummaryService
}

func (s *Scheduler) AddSummaryJob(schedule string) {
	_, err := s.Scheduler.Cron(schedule).Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		
		s.logger.Info("要約生成ジョブを開始します", zap.Any("context", ctx))
		// TODO: データベースから未要約の記事を取得し、要約処理を実行
		// s.summaryService.GenerateAndStoreSummary(ctx, articleID, content)
	})
	
	if err != nil {
		s.logger.Error("要約ジョブの追加に失敗しました",
			zap.String("schedule", schedule),
			zap.Error(err))
	}
}

func (s *Scheduler) AddTask(schedule string, task func()) {
	_, err := s.Scheduler.Cron(schedule).Do(task)
	if err != nil {
		s.logger.Error("タスクの追加に失敗しました",
			zap.String("schedule", schedule),
			zap.Error(err))
	}
}

func NewScheduler(
	discord *discordgo.Session, 
	logger *zap.Logger,
	summaryService *SummaryService,
) *Scheduler {
	s := gocron.NewScheduler(time.UTC)
	return &Scheduler{
		Scheduler:      s,
		logger:         logger,
		summaryService: summaryService,
	}
}

func (s *Scheduler) Start() {
	s.Scheduler.StartAsync()
	s.logger.Info("スケジューラを起動しました")
}

func WaitForShutdown(logger *zap.Logger) {
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	logger.Info("シャットダウン信号を受信しました")
}
