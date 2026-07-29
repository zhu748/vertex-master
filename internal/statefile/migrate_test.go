package statefile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMigrateMovesLegacyFile(t *testing.T) {
	root := t.TempDir()
	oldPath := filepath.Join(root, "config", ".rules_agreed")
	newPath := filepath.Join(root, "config", "state", ".rules_agreed")
	if err := os.MkdirAll(filepath.Dir(oldPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(oldPath, []byte("agreement"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := Migrate(oldPath, newPath); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	data, err := os.ReadFile(newPath)
	if err != nil {
		t.Fatalf("read migrated file: %v", err)
	}
	if string(data) != "agreement" {
		t.Fatalf("migrated data = %q, want %q", data, "agreement")
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("legacy file still exists or stat failed unexpectedly: %v", err)
	}
}

func TestMigratePreservesExistingDestination(t *testing.T) {
	root := t.TempDir()
	oldPath := filepath.Join(root, "legacy")
	newPath := filepath.Join(root, "state", "current")
	if err := os.MkdirAll(filepath.Dir(newPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(oldPath, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newPath, []byte("current"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := Migrate(oldPath, newPath); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	data, err := os.ReadFile(newPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "current" {
		t.Fatalf("destination data = %q, want %q", data, "current")
	}
	if _, err := os.Stat(oldPath); err != nil {
		t.Fatalf("legacy file should remain when destination already exists: %v", err)
	}
}

func TestMigrateMissingSourceIsNoop(t *testing.T) {
	root := t.TempDir()
	if err := Migrate(filepath.Join(root, "missing"), filepath.Join(root, "state", "current")); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
}

func TestMigrateRejectsDirectorySource(t *testing.T) {
	root := t.TempDir()
	oldPath := filepath.Join(root, "legacy")
	if err := os.Mkdir(oldPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(oldPath, filepath.Join(root, "state", "current")); err == nil {
		t.Fatal("Migrate() error = nil, want directory source error")
	}
}
