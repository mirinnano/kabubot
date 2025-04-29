package status
import (
	"fmt"
	"os"
	"time"

	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/mem"
)

type SystemStats struct {
	Hostname        string
	MemoryPercent   float64
	CPUPercent      float64
	LatestNewsTime  string
}

// scrape した記事リストから最新日時を取る例。
// articles は []ScrapedArticle の場合です。
func getLatestNewsTime(articles []ScrapedArticle) string {
	if len(articles) == 0 {
			return "不明"
	}
	latest := articles[0].PublishedAt
	for _, art := range articles {
			if art.PublishedAt.After(latest) {
					latest = art.PublishedAt
			}
	}
	return latest.Format(time.RFC3339)
}

func fetchSystemStats(articles []ScrapedArticle) SystemStats {
	hostname, _ := os.Hostname()

	vm, _ := mem.VirtualMemory()
	mPct := vm.UsedPercent

	cpuPercents, _ := cpu.Percent(0, false)
	cPct := cpuPercents[0]

	return SystemStats{
			Hostname:       hostname,
			MemoryPercent:  mPct,
			CPUPercent:     cPct,
			LatestNewsTime: getLatestNewsTime(articles),
	}
}
