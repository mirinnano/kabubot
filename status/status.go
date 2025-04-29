package status

import (
	"fmt"
	"os"
	

	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/mem"
	"github.com/bwmarrin/discordgo"
)

// SystemStats はシステム稼働状況を保持します。
type SystemStats struct {
	Hostname      string  // ホスト名
	MemoryPercent float64 // メモリ使用率 (%%)
	CPUPercent    float64 // CPU 使用率 (%%)
}

// FetchSystemStats はシステム情報を取得し、SystemStats 構造体として返します。
func FetchSystemStats() (SystemStats, error) {
	hostname, err := os.Hostname()
	if err != nil {
		return SystemStats{}, err
	}

	vm, err := mem.VirtualMemory()
	if err != nil {
		return SystemStats{}, err
	}

	cpuPercents, err := cpu.Percent(0, false)
	if err != nil {
		return SystemStats{}, err
	}

	return SystemStats{
		Hostname:      hostname,
		MemoryPercent: vm.UsedPercent,
		CPUPercent:    cpuPercents[0],
	}, nil
}

// buildStatusText は Discord のプロフィール "Playing" 用文字列を生成します。
func buildStatusText(stats SystemStats) string {
	// 例: "Mem:45.3% | CPU:10.1% | host123"
	return fmt.Sprintf(
		"Mem:%.1f%% | CPU:%.1f%% | %s @aria_math",
		stats.MemoryPercent,
		stats.CPUPercent,
		stats.Hostname,
	)
}

// UpdatePlayingStatus は指定セッションのDiscordステータスを "Playing" に更新します。
func UpdatePlayingStatus(s *discordgo.Session) error {
	stats, err := FetchSystemStats()
	if err != nil {
		return err
	}
	statusText := buildStatusText(stats)
	return s.UpdateStatusComplex(discordgo.UpdateStatusData{
		Activities: []*discordgo.Activity{{
			Name: statusText,
			Type: discordgo.ActivityTypeGame,
		}},
		Status: "online",
	})
}