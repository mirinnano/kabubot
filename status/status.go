package status

import (
	"fmt"
	"sync"
	"time"
"os"
	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/mem"
	"github.com/bwmarrin/discordgo"
	"go.uber.org/zap"
)

type SystemStats struct {
	Hostname      string  // キャッシュ用
	MemoryPercent float64 // 直近キャッシュ
	CPUPercent    float64 // 直近キャッシュ
}

var (
	stats      SystemStats
	statsMutex sync.RWMutex
	logger     *zap.Logger
)

// StartStatsCollector をアプリ起動時に一度呼び出してください。
func StartStatsCollector(log *zap.Logger) {
	logger = log

	// ホスト名は一度だけ取得
	host, err := os.Hostname()
	if err != nil {
			logger.Warn("ホスト名取得に失敗", zap.Error(err))
	}
	statsMutex.Lock()
	stats.Hostname = host
	statsMutex.Unlock()

	// 10秒間隔でメトリクス取得
	go func() {
			ticker := time.NewTicker(10 * time.Second)
			defer ticker.Stop()

			for range ticker.C {
					refreshStats()
			}
	}()
}

func refreshStats() {
	// メモリ使用率
	vm, err := mem.VirtualMemory()
	if err != nil {
			logger.Warn("VirtualMemory 取得エラー", zap.Error(err))
			return
	}

	// CPU使用率（前回からの差分ではなく瞬間値を取得）
	cpuPercents, err := cpu.Percent(0, false)
	if err != nil {
			logger.Warn("CPU Percent 取得エラー", zap.Error(err))
			return
	}

	statsMutex.Lock()
	stats.MemoryPercent = vm.UsedPercent
	if len(cpuPercents) > 0 {
			stats.CPUPercent = cpuPercents[0]
	}
	statsMutex.Unlock()
}

// UpdatePlayingStatus は Discord の Playing ステータスを更新します。
// 内部で重い処理は行わず、キャッシュをフォーマットするだけ。
func UpdatePlayingStatus(s *discordgo.Session) error {
	statsMutex.RLock()
	st := stats
	statsMutex.RUnlock()

	activity := &discordgo.Activity{
			Name: fmt.Sprintf("Mem:%.1f%% | CPU:%.1f%% | %s", st.MemoryPercent, st.CPUPercent, st.Hostname),
			Type: discordgo.ActivityTypeGame,
	}

	return s.UpdateStatusComplex(discordgo.UpdateStatusData{
			Activities: []*discordgo.Activity{activity},
			Status:     "online",
	})
}
