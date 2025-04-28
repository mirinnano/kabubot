package services

import (
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
	logger *zap.Logger
}

func (s *Scheduler) AddTask(schedule string, task func()) {
	_, err := s.Scheduler.Cron(schedule).Do(task)
	if err != nil {
		s.logger.Error("タスクの追加に失敗しました",
			zap.String("schedule", schedule),
			zap.Error(err))
	}
}

func NewScheduler(discord *discordgo.Session, logger *zap.Logger) *Scheduler {
	s := gocron.NewScheduler(time.UTC)
	return &Scheduler{
		Scheduler: s,
		logger:    logger,
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
