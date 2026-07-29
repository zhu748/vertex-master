package logger

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestDailyLogger(t *testing.T) {
	tmpDir := t.TempDir()
	logDir := filepath.Join(tmpDir, "logs")

	logger := NewDailyLogger(logDir)
	defer func() {
		if logger.latestFd != nil {
			_ = logger.latestFd.Close()
		}
	}()

	// Write something
	msg := []byte("hello world\n")
	n, err := logger.Write(msg)
	if err != nil {
		t.Fatalf("Failed to write to logger: %v", err)
	}
	if n != len(msg) {
		t.Fatalf("Expected to write %d bytes, wrote %d", len(msg), n)
	}

	// Verify file is created
	latestPath := filepath.Join(logDir, "logs_latest.log")
	content, err := os.ReadFile(latestPath)
	if err != nil {
		t.Fatalf("Failed to read logs_latest.log: %v", err)
	}
	if !strings.Contains(string(content), string(msg)) {
		t.Fatalf("Expected content to contain %q, got %q", string(msg), string(content))
	}

	// Close to trigger merge
	if err := logger.Close(); err != nil {
		t.Fatal(err)
	}
	if err := logger.Close(); err != nil { // 幂等
		t.Fatal(err)
	}
	select {
	case <-logger.cleanupDone:
	default:
		t.Fatal("Close returned before cleanup goroutine stopped")
	}

	// Verify logs_latest.log is deleted (os.IsNotExist)
	_, err = os.ReadFile(latestPath)
	if err == nil {
		t.Fatalf("Expected logs_latest.log to be deleted")
	}

	nowDate := time.Now().Format("2006-01-02")
	expectedName := fmt.Sprintf("vproxy-%s.log", nowDate)

	// Read merged content
	mergedContent, err := os.ReadFile(filepath.Join(logDir, expectedName))
	if err != nil {
		t.Fatalf("Failed to read merged log file: %v", err)
	}
	if !strings.Contains(string(mergedContent), string(msg)) {
		t.Fatalf("Expected merged content to contain %q, got %q", string(msg), string(mergedContent))
	}

	// Create some old files to test cleanup
	oldDate1 := time.Now().Add(-8 * 24 * time.Hour)
	oldFile1 := filepath.Join(logDir, fmt.Sprintf("vproxy-%s.log", oldDate1.Format("2006-01-02")))
	if err := os.WriteFile(oldFile1, []byte("old log 1"), 0644); err != nil {
		t.Fatalf("Failed to create old log file: %v", err)
	}
	_ = os.Chtimes(oldFile1, oldDate1, oldDate1) // Ensure mod time is old

	oldDate2 := time.Now().Add(-6 * 24 * time.Hour) // This one should NOT be deleted (within 7 days)
	oldFile2 := filepath.Join(logDir, fmt.Sprintf("vproxy-%s.log", oldDate2.Format("2006-01-02")))
	if err := os.WriteFile(oldFile2, []byte("old log 2"), 0644); err != nil {
		t.Fatalf("Failed to create old log file: %v", err)
	}
	_ = os.Chtimes(oldFile2, oldDate2, oldDate2)
	customOldFile := filepath.Join(logDir, "custom.log")
	if err := os.WriteFile(customOldFile, []byte("must remain"), 0644); err != nil {
		t.Fatal(err)
	}
	_ = os.Chtimes(customOldFile, oldDate1, oldDate1)

	// Call cleanup manually
	logger.cleanup()

	// Verify cleanup
	entries, _ := os.ReadDir(logDir)
	var filenames []string
	for _, e := range entries {
		filenames = append(filenames, e.Name())
	}

	// oldFile1 should be deleted, oldFile2 and current file should remain.
	foundOld1 := false
	foundOld2 := false
	for _, name := range filenames {
		if name == filepath.Base(oldFile1) {
			foundOld1 = true
		}
		if name == filepath.Base(oldFile2) {
			foundOld2 = true
		}
	}

	if foundOld1 {
		t.Fatalf("Cleanup failed to delete old file: %s", oldFile1)
	}
	if !foundOld2 {
		t.Fatalf("Cleanup incorrectly deleted recent file: %s", oldFile2)
	}
	if _, err := os.Stat(customOldFile); err != nil {
		t.Fatalf("cleanup deleted unrelated log file: %v", err)
	}
}

func TestDailyLoggerPreservesLatestWhenArchiveTargetCannotOpen(t *testing.T) {
	logDir := t.TempDir()
	logger := NewDailyLogger(logDir)
	message := []byte("preserve this log\n")
	if _, err := logger.Write(message); err != nil {
		t.Fatal(err)
	}
	targetPath := filepath.Join(logDir, fmt.Sprintf("vproxy-%s.log", time.Now().Format("2006-01-02")))
	if err := os.Mkdir(targetPath, 0755); err != nil {
		t.Fatal(err)
	}
	if err := logger.Close(); err == nil {
		t.Fatal("Close succeeded even though archive target is a directory")
	}
	latest, err := os.ReadFile(filepath.Join(logDir, "logs_latest.log"))
	if err != nil {
		t.Fatalf("latest log was removed after archive failure: %v", err)
	}
	if !bytes.Contains(latest, message) {
		t.Fatalf("latest log lost content after archive failure: %q", latest)
	}
}

func TestDailyLoggerStreamsAppendIntoExistingArchive(t *testing.T) {
	logDir := t.TempDir()
	targetPath := filepath.Join(logDir, fmt.Sprintf("vproxy-%s.log", time.Now().Format("2006-01-02")))
	prefix := []byte("existing archive\n")
	if err := os.WriteFile(targetPath, prefix, 0644); err != nil {
		t.Fatal(err)
	}
	logger := NewDailyLogger(logDir)
	chunk := bytes.Repeat([]byte("0123456789abcdef"), 4096)
	const writes = 128
	for range writes {
		if _, err := logger.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	if err := logger.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	startupLen := len(fmt.Sprintf("\n========== STARTUP: %s (Timestamp: %d) ==========\n", time.Now().Format("2006-01-02 15:04:05"), time.Now().Unix()))
	minimumSize := int64(len(prefix) + startupLen + len(chunk)*writes)
	if info.Size() < minimumSize {
		t.Fatalf("archive size = %d, want at least %d", info.Size(), minimumSize)
	}
	if _, err := os.Stat(filepath.Join(logDir, "logs_latest.log")); !os.IsNotExist(err) {
		t.Fatalf("latest log still exists after successful archive: %v", err)
	}
}

func TestDailyLoggerRotatesLatestAtBoundedSize(t *testing.T) {
	logDir := t.TempDir()
	logger := NewDailyLogger(logDir)
	t.Cleanup(func() { _ = logger.Close() })

	// Avoid allocating a 64 MiB fixture: the write path uses the tracked size,
	// while the rotation itself still exercises close/archive/reopen end to end.
	logger.mu.Lock()
	logger.latestSize = maxLatestLogBytes
	logger.mu.Unlock()

	const message = "after-size-rotation\n"
	if _, err := logger.Write([]byte(message)); err != nil {
		t.Fatal(err)
	}
	latest, err := os.ReadFile(filepath.Join(logDir, "logs_latest.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(latest), message) {
		t.Fatalf("rotated latest log lost new write: %q", latest)
	}

	entries, err := os.ReadDir(logDir)
	if err != nil {
		t.Fatal(err)
	}
	foundArchive := false
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "vproxy-") &&
			strings.HasSuffix(entry.Name(), ".log") {
			foundArchive = true
			break
		}
	}
	if !foundArchive {
		t.Fatalf("size rotation did not create an archive: %v", entries)
	}
}

func TestDailyLoggerUsesPrivatePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not expose Unix permission semantics")
	}
	logDir := filepath.Join(t.TempDir(), "logs")
	logger := NewDailyLogger(logDir)
	defer func() { _ = logger.Close() }()

	dirInfo, err := os.Stat(logDir)
	if err != nil {
		t.Fatal(err)
	}
	if got := dirInfo.Mode().Perm(); got != 0o700 {
		t.Fatalf("log directory permissions=%#o, want 0700", got)
	}
	fileInfo, err := os.Stat(filepath.Join(logDir, "logs_latest.log"))
	if err != nil {
		t.Fatal(err)
	}
	if got := fileInfo.Mode().Perm(); got != 0o600 {
		t.Fatalf("latest log permissions=%#o, want 0600", got)
	}
}

func TestDailyLoggerCapsRecentArchiveBytes(t *testing.T) {
	logDir := t.TempDir()
	logger := &DailyLogger{logDir: logDir} //nolint:exhaustruct
	now := time.Now()
	const fileSize = 40
	for index := range 4 {
		path := filepath.Join(logDir, fmt.Sprintf("vproxy-retained-%d.log", index))
		if err := os.WriteFile(path, bytes.Repeat([]byte{'x'}, fileSize), 0o600); err != nil {
			t.Fatal(err)
		}
		modified := now.Add(time.Duration(index) * time.Minute)
		if err := os.Chtimes(path, modified, modified); err != nil {
			t.Fatal(err)
		}
	}

	logger.cleanupWithLimit(3 * fileSize)

	if _, err := os.Stat(filepath.Join(logDir, "vproxy-retained-0.log")); !os.IsNotExist(err) {
		t.Fatalf("oldest archive should be removed first: %v", err)
	}
	for index := 1; index < 4; index++ {
		if _, err := os.Stat(filepath.Join(logDir, fmt.Sprintf("vproxy-retained-%d.log", index))); err != nil {
			t.Fatalf("recent archive %d should remain: %v", index, err)
		}
	}
}
