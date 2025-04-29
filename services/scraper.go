package services

import (
	"go.uber.org/zap"
)

type ScraperService struct {
	logger *zap.Logger
}

func NewScraperService(logger *zap.Logger) *ScraperService {
	return &ScraperService{
		logger: logger,
	}
}
