package logger

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// DailyLogger writes to logs_latest.log, rotating on date/size boundaries and
// archiving the final active file on Close.
type DailyLogger struct {
	mu          sync.Mutex
	logDir      string
	latestFd    *os.File
	currentDate string
	latestSize  int64
	rotationSeq uint64
	cleanupStop chan struct{}
	cleanupDone chan struct{}
	closeOnce   sync.Once
	closeErr    error
}

const (
	maxLatestLogBytes   = 64 << 20
	maxRetainedLogBytes = 512 << 20
)

// NewDailyLogger creates a new DailyLogger that writes logs to the specified directory.
func NewDailyLogger(dir string) *DailyLogger {
	_ = os.MkdirAll(dir, 0o700)
	_ = os.Chmod(dir, 0o700)

	latestPath := filepath.Join(dir, "logs_latest.log")
	f, _ := os.OpenFile(latestPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	_ = os.Chmod(latestPath, 0o600)

	latestSize := int64(0)
	if f != nil {
		now := time.Now()
		startupMsg := fmt.Sprintf("\n========== STARTUP: %s (Timestamp: %d) ==========\n", now.Format("2006-01-02 15:04:05"), now.Unix())
		if written, err := f.WriteString(startupMsg); err == nil {
			latestSize += int64(written)
		}
		if info, err := f.Stat(); err == nil {
			latestSize = info.Size()
		}
	}

	dl := &DailyLogger{
		logDir:      dir,
		latestFd:    f,
		currentDate: time.Now().Format("2006-01-02"),
		latestSize:  latestSize,
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
	now := time.Now()
	date := now.Format("2006-01-02")
	if date != l.currentDate ||
		(l.latestSize > 0 && l.latestSize+int64(len(p)) > maxLatestLogBytes) {
		if err := l.rotateLocked(now, date != l.currentDate); err != nil {
			return 0, err
		}
	}
	n, err = l.latestFd.Write(p)
	l.latestSize += int64(n)
	return n, err
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

	archiveDate := l.currentDate
	if archiveDate == "" {
		archiveDate = time.Now().Format("2006-01-02")
	}
	targetPath := filepath.Join(l.logDir, fmt.Sprintf("vproxy-%s.log", archiveDate))
	return l.archiveLatestLocked(targetPath)
}

func (l *DailyLogger) archiveLatestLocked(targetPath string) error {
	latestPath := filepath.Join(l.logDir, "logs_latest.log")
	if _, latestErr := os.Stat(latestPath); latestErr == nil {
		if _, targetErr := os.Stat(targetPath); os.IsNotExist(targetErr) {
			// 当天首次归档可直接原子重命名，不复制日志内容，也不产生与文件
			// 大小成正比的内存峰值。
			if err := os.Rename(latestPath, targetPath); err == nil {
				_ = os.Chmod(targetPath, 0o600)
				return nil
			}
		}
	} else if !os.IsNotExist(latestErr) {
		return fmt.Errorf("stat latest log: %w", latestErr)
	}

	target, err := os.OpenFile(targetPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open daily log: %w", err)
	}
	_ = os.Chmod(targetPath, 0o600)
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

func (l *DailyLogger) rotateLocked(now time.Time, dateChanged bool) error {
	if l.latestFd != nil {
		if err := l.latestFd.Close(); err != nil {
			return fmt.Errorf("close latest log for rotation: %w", err)
		}
		l.latestFd = nil
	}

	archiveDate := l.currentDate
	if archiveDate == "" {
		archiveDate = now.Format("2006-01-02")
	}
	targetPath := filepath.Join(l.logDir, fmt.Sprintf("vproxy-%s.log", archiveDate))
	if !dateChanged {
		l.rotationSeq++
		targetPath = filepath.Join(
			l.logDir,
			fmt.Sprintf(
				"vproxy-%s-%s-%06d.log",
				archiveDate,
				now.Format("150405"),
				l.rotationSeq,
			),
		)
	}
	archiveErr := l.archiveLatestLocked(targetPath)

	latestPath := filepath.Join(l.logDir, "logs_latest.log")
	fd, openErr := os.OpenFile(latestPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if fd != nil {
		_ = os.Chmod(latestPath, 0o600)
		l.latestFd = fd
		if info, err := fd.Stat(); err == nil {
			l.latestSize = info.Size()
		} else {
			l.latestSize = 0
		}
	} else {
		l.latestSize = 0
	}
	l.currentDate = now.Format("2006-01-02")
	return errors.Join(archiveErr, openErr)
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
	l.cleanupWithLimit(maxRetainedLogBytes)
}

func (l *DailyLogger) cleanupWithLimit(maxRetainedBytes int64) {
	entries, err := os.ReadDir(l.logDir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-7 * 24 * time.Hour)
	type retainedLog struct {
		path    string
		modTime time.Time
		size    int64
	}
	retained := make([]retainedLog, 0, len(entries))
	totalBytes := int64(0)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "vproxy-") ||
			!strings.HasSuffix(entry.Name(), ".log") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		path := filepath.Join(l.logDir, entry.Name())
		if info.ModTime().Before(cutoff) {
			_ = os.Remove(path)
			continue
		}
		retained = append(retained, retainedLog{
			path: path, modTime: info.ModTime(), size: info.Size(),
		})
		totalBytes += info.Size()
	}
	if totalBytes <= maxRetainedBytes {
		return
	}
	sort.Slice(retained, func(i, j int) bool {
		return retained[i].modTime.Before(retained[j].modTime)
	})
	for _, item := range retained {
		if totalBytes <= maxRetainedBytes {
			break
		}
		if err := os.Remove(item.path); err == nil || os.IsNotExist(err) {
			totalBytes -= item.size
		}
	}
}
