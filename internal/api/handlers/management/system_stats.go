package management

import (
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/api/middleware"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/disk"
	"github.com/shirou/gopsutil/v3/mem"
	psnet "github.com/shirou/gopsutil/v3/net"
	"github.com/shirou/gopsutil/v3/process"
	log "github.com/sirupsen/logrus"
)

// SystemStats is the JSON payload pushed via WebSocket and returned by HTTP.
type SystemStats struct {
	// Database
	DBSizeBytes int64  `json:"db_size_bytes"`
	DBEngine    string `json:"db_engine"`

	// Request log body storage inside the runtime database.
	LogContentStoreBytes int64 `json:"log_content_store_bytes"`

	// Log directory size on disk.
	LogDirSizeBytes int64 `json:"log_dir_size_bytes"`

	// Deprecated alias retained for older panels that still read log_size_bytes.
	LogSizeBytes int64 `json:"log_size_bytes"`

	// Process-level resources
	ProcessMemBytes uint64  `json:"process_mem_bytes"`
	ProcessMemPct   float64 `json:"process_mem_pct"`
	ProcessCPUPct   float64 `json:"process_cpu_pct"`
	GoRoutines      int     `json:"go_routines"`
	GoHeapBytes     uint64  `json:"go_heap_bytes"`

	// System-level resources
	SystemCPUPct   float64 `json:"system_cpu_pct"`
	SystemMemTotal uint64  `json:"system_mem_total"`
	SystemMemUsed  uint64  `json:"system_mem_used"`
	SystemMemPct   float64 `json:"system_mem_pct"`

	// Network (service-level)
	NetBytesSent uint64  `json:"net_bytes_sent"`
	NetBytesRecv uint64  `json:"net_bytes_recv"`
	NetSendRate  float64 `json:"net_send_rate"` // bytes/sec
	NetRecvRate  float64 `json:"net_recv_rate"` // bytes/sec

	// Disk
	DiskTotal uint64  `json:"disk_total"`
	DiskUsed  uint64  `json:"disk_used"`
	DiskFree  uint64  `json:"disk_free"`
	DiskPct   float64 `json:"disk_pct"`

	// Uptime
	UptimeSeconds int64  `json:"uptime_seconds"`
	StartTime     string `json:"start_time"`

	// Channel latency
	ChannelLatency []usage.ChannelLatency `json:"channel_latency"`

	// Concurrency
	ActiveConcurrency []middleware.ConcurrencySnapshot `json:"active_concurrency"`
	TotalInFlight     int64                            `json:"total_in_flight"`
	TotalRPM          int                              `json:"total_rpm"`
	TotalTPM          int64                            `json:"total_tpm"`
}

// network baseline for rate calculation
var (
	netMu         sync.Mutex
	lastNetSample time.Time
	lastBytesSent uint64
	lastBytesRecv uint64
)

func (h *Handler) collectSystemStats() SystemStats {
	stats := SystemStats{
		GoRoutines:    runtime.NumGoroutine(),
		StartTime:     h.startTime.Format(time.RFC3339),
		UptimeSeconds: int64(time.Since(h.startTime).Seconds()),
	}

	// ── Go runtime memory ──
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	stats.GoHeapBytes = m.HeapAlloc

	dbStats, err := usage.GetDatabaseStats()
	stats.DBEngine = dbStats.Driver
	stats.DBSizeBytes = dbStats.SizeBytes
	if err != nil {
		log.Warnf("system-stats: failed to query database stats: %v", err)
	}
	if contentBytes, err := usage.GetRequestLogStorageBytes(); err == nil {
		stats.LogContentStoreBytes = contentBytes
	} else {
		log.Warnf("system-stats: failed to query request log storage bytes: %v", err)
	}

	// ── Log directory size ──
	if h.logDir != "" {
		stats.LogDirSizeBytes = dirSize(h.logDir)
		stats.LogSizeBytes = stats.LogDirSizeBytes
	}

	// ── Process CPU/Memory (gopsutil) ──
	if proc, err := process.NewProcess(int32(os.Getpid())); err == nil {
		if pct, err := proc.CPUPercent(); err == nil {
			stats.ProcessCPUPct = pct
		}
		if memInfo, err := proc.MemoryInfo(); err == nil {
			stats.ProcessMemBytes = memInfo.RSS
		}
		if pct, err := proc.MemoryPercent(); err == nil {
			stats.ProcessMemPct = float64(pct)
		}
	}

	// ── System CPU ──
	if pcts, err := cpu.Percent(0, false); err == nil && len(pcts) > 0 {
		stats.SystemCPUPct = pcts[0]
	}

	// ── System Memory ──
	if vm, err := mem.VirtualMemory(); err == nil {
		stats.SystemMemTotal = vm.Total
		stats.SystemMemUsed = vm.Used
		stats.SystemMemPct = vm.UsedPercent
	}

	// ── Network I/O ──
	if counters, err := psnet.IOCounters(false); err == nil && len(counters) > 0 {
		total := counters[0]
		stats.NetBytesSent = total.BytesSent
		stats.NetBytesRecv = total.BytesRecv

		netMu.Lock()
		now := time.Now()
		if !lastNetSample.IsZero() {
			elapsed := now.Sub(lastNetSample).Seconds()
			if elapsed > 0 {
				stats.NetSendRate = float64(total.BytesSent-lastBytesSent) / elapsed
				stats.NetRecvRate = float64(total.BytesRecv-lastBytesRecv) / elapsed
			}
		}
		lastNetSample = now
		lastBytesSent = total.BytesSent
		lastBytesRecv = total.BytesRecv
		netMu.Unlock()
	}

	// ── Disk usage ──
	if du, err := disk.Usage("/"); err == nil {
		stats.DiskTotal = du.Total
		stats.DiskUsed = du.Used
		stats.DiskFree = du.Free
		stats.DiskPct = du.UsedPercent
	}

	// ── Channel latency (from DB) ──
	if cl, err := usage.GetChannelAvgLatency(7); err == nil {
		stats.ChannelLatency = cl
	}

	// ── Concurrency snapshot ──
	stats.ActiveConcurrency, stats.TotalInFlight = middleware.GetConcurrencySnapshot()

	// Compute system-wide RPM and TPM totals
	var sysRPM int
	var sysTPM int64
	for _, snap := range stats.ActiveConcurrency {
		sysRPM += snap.RPM
		sysTPM += snap.TPM
	}
	stats.TotalRPM = sysRPM
	stats.TotalTPM = sysTPM

	return stats
}

// GetSystemStats handles GET /v0/management/system-stats
func (h *Handler) GetSystemStats(c *gin.Context) {
	c.JSON(http.StatusOK, h.collectSystemStats())
}

// dirSize calculates the total size of all files in a directory tree.
func dirSize(path string) int64 {
	var size int64
	_ = filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		size += info.Size()
		return nil
	})
	return size
}
