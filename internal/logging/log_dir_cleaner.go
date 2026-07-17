package logging

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

const logDirCleanerInterval = time.Minute

var logDirCleanerCancel context.CancelFunc

func configureLogDirCleanerLocked(logDir string, maxTotalSizeMB int, protectedPath string) {
	stopLogDirCleanerLocked()

	if maxTotalSizeMB <= 0 {
		return
	}

	maxBytes := int64(maxTotalSizeMB) * 1024 * 1024
	if maxBytes <= 0 {
		return
	}

	dir := strings.TrimSpace(logDir)
	if dir == "" {
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	logDirCleanerCancel = cancel
	// 日志目录清理器属于 logging 子系统自身的后台维护任务：
	// - owner: logging 配置生命周期
	// - 取消条件: stopLogDirCleanerLocked / 下一次重新配置 / 进程退出
	// - 超时策略: 无单次请求超时，按 ticker 周期执行
	// - 清理方式: cancel 后退出 runLogDirCleaner 循环，不依赖外部请求 context
	go runLogDirCleaner(ctx, filepath.Clean(dir), maxBytes, strings.TrimSpace(protectedPath))
}

func stopLogDirCleanerLocked() {
	if logDirCleanerCancel == nil {
		return
	}
	logDirCleanerCancel()
	logDirCleanerCancel = nil
}

func runLogDirCleaner(ctx context.Context, logDir string, maxBytes int64, protectedPath string) {
	ticker := time.NewTicker(logDirCleanerInterval)
	defer ticker.Stop()

	cleanOnce := func() {
		deleted, errClean := enforceLogDirSizeLimit(logDir, maxBytes, protectedPath)
		if errClean != nil {
			log.WithError(errClean).Warn("logging: failed to enforce log directory size limit")
			return
		}
		if deleted > 0 {
			log.Debugf("logging: removed %d old log file(s) to enforce log directory size limit", deleted)
		}
	}

	cleanOnce()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cleanOnce()
		}
	}
}

func enforceLogDirSizeLimit(logDir string, maxBytes int64, protectedPath string) (int, error) {
	if maxBytes <= 0 {
		return 0, nil
	}

	dir := strings.TrimSpace(logDir)
	if dir == "" {
		return 0, nil
	}
	dir = filepath.Clean(dir)

	entries, errRead := os.ReadDir(dir)
	if errRead != nil {
		if os.IsNotExist(errRead) {
			return 0, nil
		}
		return 0, errRead
	}

	protected := strings.TrimSpace(protectedPath)
	if protected != "" {
		protected = filepath.Clean(protected)
	}

	type logFile struct {
		path    string
		size    int64
		modTime time.Time
	}

	var (
		files []logFile
		total int64
	)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !isLogDirManagedFileName(name) {
			continue
		}
		info, errInfo := entry.Info()
		if errInfo != nil {
			continue
		}
		if !info.Mode().IsRegular() {
			continue
		}
		path := filepath.Join(dir, name)
		files = append(files, logFile{
			path:    path,
			size:    info.Size(),
			modTime: info.ModTime(),
		})
		total += info.Size()
	}

	if total <= maxBytes {
		return 0, nil
	}

	// Prefer deleting orphaned spool temps before request/main logs so a
	// leaked *.tmp storm cannot pin the directory above the configured cap.
	sort.Slice(files, func(i, j int) bool {
		iTmp := isLogSpoolTempFileName(filepath.Base(files[i].path))
		jTmp := isLogSpoolTempFileName(filepath.Base(files[j].path))
		if iTmp != jTmp {
			return iTmp
		}
		return files[i].modTime.Before(files[j].modTime)
	})

	deleted := 0
	for _, file := range files {
		if total <= maxBytes {
			break
		}
		if protected != "" && filepath.Clean(file.path) == protected {
			continue
		}
		if errRemove := os.Remove(file.path); errRemove != nil {
			// Active stream may still hold the spool file open; skip and retry next cycle.
			if isLogSpoolTempFileName(filepath.Base(file.path)) && !os.IsNotExist(errRemove) {
				continue
			}
			log.WithError(errRemove).Warnf("logging: failed to remove old log file: %s", filepath.Base(file.path))
			continue
		}
		total -= file.size
		deleted++
	}

	return deleted, nil
}

func isLogFileName(name string) bool {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return false
	}
	lower := strings.ToLower(trimmed)
	return strings.HasSuffix(lower, ".log") || strings.HasSuffix(lower, ".log.gz")
}

// isLogSpoolTempFileName matches request/response body spool files written into
// the logs directory while assembling per-request logs. These used to escape
// logs-max-total-size-mb because only *.log / *.log.gz were counted.
func isLogSpoolTempFileName(name string) bool {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return false
	}
	lower := strings.ToLower(trimmed)
	if !strings.HasSuffix(lower, ".tmp") {
		return false
	}
	return strings.HasPrefix(lower, "request-body-") || strings.HasPrefix(lower, "response-body-")
}

func isLogDirManagedFileName(name string) bool {
	return isLogFileName(name) || isLogSpoolTempFileName(name)
}
