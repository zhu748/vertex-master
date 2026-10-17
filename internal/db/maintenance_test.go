package db

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestMaintenanceUsesExistingDatabase(t *testing.T) {
	CloseDB()
	if err := InitDB(filepath.Join(t.TempDir(), "maintenance.db")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(CloseDB)
	database := CurrentDB()
	if _, err := database.Exec(`INSERT INTO nodes(raw_uri,type,name) VALUES ('http://example.com:80','http','keep-me')`); err != nil {
		t.Fatal(err)
	}
	m := NewMaintenance(database)
	stats, err := m.Stats(context.Background())
	if err != nil || stats.PageSize <= 0 || stats.PageCount <= 0 {
		t.Fatalf("stats=%+v err=%v", stats, err)
	}
	check, err := m.Check(context.Background())
	if err != nil || len(check) != 1 || check[0] != "ok" {
		t.Fatalf("check=%v err=%v", check, err)
	}
	if _, err := m.Checkpoint(context.Background()); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := database.QueryRow("SELECT count(*) FROM nodes").Scan(&count); err != nil || count != 1 {
		t.Fatalf("maintenance lost data: count=%d err=%v", count, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := m.Stats(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled stats: %v", err)
	}
}

func TestMaintenanceWithoutDatabase(t *testing.T) {
	m := NewMaintenance(nil)
	if _, err := m.Stats(context.Background()); !errors.Is(err, ErrNotInitialized) {
		t.Fatal(err)
	}
	if _, err := m.Check(context.Background()); !errors.Is(err, ErrNotInitialized) {
		t.Fatal(err)
	}
	if _, err := m.Checkpoint(context.Background()); !errors.Is(err, ErrNotInitialized) {
		t.Fatal(err)
	}
}
