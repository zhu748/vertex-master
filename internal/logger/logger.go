package logger

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// DailyLogger implements an io.Writer that writes to logs_latest.log.
// On Close, it appends the content to a daily log file and clears logs_latest.log.
type DailyLogger struct {
	mu          sync.Mutex
	logDir      string
	latestFd    *os.File
	cleanupStop chan struct{}
	cleanupDone chan struct{}
	closeOnce   sync.Once
	closeErr    error
}

// NewDailyLogger creates a new DailyLogger that writes logs to the specified directory.
func NewDailyLogger(dir string) *DailyLogger {
	_ = os.MkdirAll(dir, 0755)

	latestPath := filepath.Join(dir, "logs_latest.log")
	f, _ := os.OpenFile(latestPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)

	if f != nil {
		now := time.Now()
		startupMsg := fmt.Sprintf("\n========== STARTUP: %s (Timestamp: %d) ==========\n", now.Format("2006-01-02 15:04:05"), now.Unix())
		_, _ = f.WriteString(startupMsg)
	}

	dl := &DailyLogger{
		logDir:      dir,
		latestFd:    f,
		cleanupStop: make(chan struct{}),
		cleanupDone: make(chan struct{}),
	}
	go dl.cleanupRoutine()
	return dl
}

func (l *DailyLogger) Write(p []byte) (n int, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.latestFd == nil {
		return 0, fmt.Errorf("logger closed")
	}
	return l.latestFd.Write(p)
}

func (l *DailyLogger) Close() error {
	l.closeOnce.Do(func() {
		close(l.cleanupStop)
		<-l.cleanupDone
		l.closeErr = l.closeAndArchive()
	})
	return l.closeErr
}

func (l *DailyLogger) closeAndArchive() error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.latestFd != nil {
		if err := l.latestFd.Close(); err != nil {
			return fmt.Errorf("close latest log: %w", err)
		}
		l.latestFd = nil
	}

	latestPath := filepath.Join(l.logDir, "logs_latest.log")
	nowDate := time.Now().Format("2006-01-02")
	targetPath := filepath.Join(l.logDir, fmt.Sprintf("vproxy-%s.log", nowDate))

	if _, latestErr := os.Stat(latestPath); latestErr == nil {
		if _, targetErr := os.Stat(targetPath); os.IsNotExist(targetErr) {
			// 当天首次归档可直接原子重命名，不复制日志内容，也不产生与文件
			// 大小成正比的内存峰值。
			if err := os.Rename(latestPath, targetPath); err == nil {
				return nil
			}
		}
	} else if !os.IsNotExist(latestErr) {
		return fmt.Errorf("stat latest log: %w", latestErr)
	}

	target, err := os.OpenFile(targetPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	if err != nil {
		return fmt.Errorf("open daily log: %w", err)
	}
	latest, err := os.Open(latestPath)
	if os.IsNotExist(err) {
		return target.Close()
	}
	if err != nil {
		_ = target.Close()
		return fmt.Errorf("open latest log: %w", err)
	}

	buffer := make([]byte, 64*1024)
	_, copyErr := io.CopyBuffer(target, latest, buffer)
	latestCloseErr := latest.Close()
	targetCloseErr := target.Close()
	if err := errors.Join(copyErr, latestCloseErr, targetCloseErr); err != nil {
		// 归档未被完整持久化时保留 latest，避免静默丢失源日志。
		return fmt.Errorf("archive latest log: %w", err)
	}
	if err := os.Remove(latestPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove archived latest log: %w", err)
	}

	return nil
}

func (l *DailyLogger) cleanupRoutine() {
	defer close(l.cleanupDone)
	l.cleanup()
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			l.cleanup()
		case <-l.cleanupStop:
			return
		}
	}
}

func (l *DailyLogger) cleanup() {
	entries, err := os.ReadDir(l.logDir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-7 * 24 * time.Hour)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "vproxy-") ||
			!strings.HasSuffix(entry.Name(), ".log") {
			continue
		}
		info, err := entry.Info()
		if err == nil && info.ModTime().Before(cutoff) {
			_ = os.Remove(filepath.Join(l.logDir, entry.Name()))
		}
	}
}
