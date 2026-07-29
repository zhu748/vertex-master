// Package statefile manages small persistent process state files.
package statefile

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Migrate moves a legacy state file to its new location without overwriting an
// existing destination. A copy-and-remove fallback handles cross-device moves.
func Migrate(oldPath, newPath string) error {
	oldPath = filepath.Clean(oldPath)
	newPath = filepath.Clean(newPath)
	if oldPath == newPath {
		return nil
	}

	if _, err := os.Stat(newPath); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("检查状态文件目标 %s: %w", newPath, err)
	}

	info, err := os.Stat(oldPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("检查旧状态文件 %s: %w", oldPath, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("旧状态路径不是普通文件: %s", oldPath)
	}

	newDir := filepath.Dir(newPath)
	if err := os.MkdirAll(newDir, 0o700); err != nil {
		return fmt.Errorf("创建状态目录 %s: %w", newDir, err)
	}
	if err := os.Rename(oldPath, newPath); err == nil {
		return nil
	}

	if err := copyAtomically(oldPath, newPath); err != nil {
		return fmt.Errorf("迁移状态文件 %s 到 %s: %w", oldPath, newPath, err)
	}
	if err := os.Remove(oldPath); err != nil {
		return fmt.Errorf("删除已迁移的旧状态文件 %s: %w", oldPath, err)
	}
	return nil
}

func copyAtomically(sourcePath, destinationPath string) (returnErr error) {
	source, err := os.Open(sourcePath)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := source.Close(); returnErr == nil && closeErr != nil {
			returnErr = closeErr
		}
	}()

	destinationDir := filepath.Dir(destinationPath)
	temp, err := os.CreateTemp(destinationDir, "."+filepath.Base(destinationPath)+".migrate-*")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer func() {
		_ = temp.Close()
		_ = os.Remove(tempPath)
	}()

	if err := temp.Chmod(0o600); err != nil {
		return err
	}
	if _, err := io.Copy(temp, source); err != nil {
		return err
	}
	if err := temp.Sync(); err != nil {
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempPath, destinationPath); err != nil {
		if _, statErr := os.Stat(destinationPath); statErr == nil {
			return nil
		}
		return err
	}
	return nil
}
