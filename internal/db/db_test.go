package db

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInitDBAndMigrate(t *testing.T) {
	CloseDB()
	tempDir, err := os.MkdirTemp("", "db_test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer func() {
		_ = os.RemoveAll(tempDir)
	}()

	dbPath := filepath.Join(tempDir, "data.db")

	// Create dummy legacy files to test migration
	nodesContent := []byte(`{
		"metadata": {"ignored": [1, {"nested": true}]},
		"nodes": [
			{"raw_uri": "http://127.0.0.1:8080", "type": "openai", "name": "Node A", "disabled": false}
		]
	}`)
	_ = os.WriteFile(filepath.Join(tempDir, "nodes.json"), nodesContent, 0644)

	healthContent := []byte(`{
		"http://127.0.0.1:8080": {
			"success_count": 10,
			"fail_count": 0,
			"consecutive_failures": 0,
			"last_test_ms": 150.5,
			"last_test_error": "",
			"last_success_at": 1670000000,
			"last_fail_at": 0,
			"cooldown_until": 0
		}
	}`)
	_ = os.WriteFile(filepath.Join(tempDir, "node_health.json"), healthContent, 0644)

	// Init DB
	if errInit := InitDB(dbPath); errInit != nil {
		t.Fatalf("Failed to InitDB: %v", errInit)
	}
	defer CloseDB()
	if got := GlobalDB.Stats().MaxOpenConnections; got != 1 {
		t.Fatalf("MaxOpenConnections = %d, want 1 to serialize SQLite writes", got)
	}

	// Verify nodes table
	var count int
	err = GlobalDB.QueryRow("SELECT COUNT(*) FROM nodes").Scan(&count)
	if err != nil || count != 1 {
		t.Errorf("Expected 1 node, got %d, error: %v", count, err)
	}

	// Verify node_health table
	var successCount int
	err = GlobalDB.QueryRow("SELECT success_count FROM node_health WHERE raw_uri = 'http://127.0.0.1:8080'").Scan(&successCount)
	if err != nil || successCount != 10 {
		t.Errorf("Expected success_count 10, got %d, error: %v", successCount, err)
	}
	for _, filename := range []string{"nodes.json", "node_health.json"} {
		if _, err := os.Stat(filepath.Join(tempDir, filename)); !os.IsNotExist(err) {
			t.Fatalf("legacy source %s should be archived, stat error=%v", filename, err)
		}
		if _, err := os.Stat(filepath.Join(tempDir, "migrated", filename+".migrated")); err != nil {
			t.Fatalf("missing archived %s: %v", filename, err)
		}
	}
}

func TestMalformedLegacyNodesRollBackAndPreserveSource(t *testing.T) {
	database := openMigrationTestDB(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "nodes.json")
	malformed := `{"nodes":[{"raw_uri":"http://good:8080","type":"http","name":"good"},{"raw_uri":]}`
	if err := os.WriteFile(path, []byte(malformed), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := migrateLegacyNodesFile(database, path, filepath.Join(dir, "migrated")); err == nil {
		t.Fatal("malformed legacy file should fail migration")
	}
	var count int
	if err := database.QueryRow("SELECT COUNT(*) FROM nodes").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("partial migration was not rolled back, nodes=%d", count)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("malformed source should be retained: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "migrated", "nodes.json.migrated")); !os.IsNotExist(err) {
		t.Fatalf("failed migration unexpectedly archived source: %v", err)
	}
}

func TestLegacyHealthWithMissingNodePreservesSource(t *testing.T) {
	database := openMigrationTestDB(t)
	if _, err := database.Exec(
		"INSERT INTO nodes(raw_uri, type, name, disabled) VALUES (?, ?, ?, ?)",
		"http://known:8080",
		"http",
		"known",
		false,
	); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "node_health.json")
	content := `{
		"http://known:8080":{"success_count":7},
		"http://missing:8080":{"success_count":9}
	}`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	migrated, skipped, err := migrateLegacyHealthFile(database, path, filepath.Join(dir, "migrated"))
	if err == nil || migrated != 1 || skipped != 1 {
		t.Fatalf("migrated=%d skipped=%d err=%v", migrated, skipped, err)
	}
	var successCount int
	if err := database.QueryRow(
		"SELECT success_count FROM node_health WHERE raw_uri = ?",
		"http://known:8080",
	).Scan(&successCount); err != nil {
		t.Fatal(err)
	}
	if successCount != 7 {
		t.Fatalf("known health success_count=%d, want 7", successCount)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("incomplete health source should be retained: %v", err)
	}
}

func TestArchiveLegacyFileDoesNotOverwritePreviousArchive(t *testing.T) {
	dir := t.TempDir()
	migratedDir := filepath.Join(dir, "migrated")
	if err := os.MkdirAll(migratedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(dir, "nodes.json")
	firstArchive := filepath.Join(migratedDir, "nodes.json.migrated")
	if err := os.WriteFile(source, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(firstArchive, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := archiveLegacyFile(source, migratedDir); err != nil {
		t.Fatal(err)
	}
	oldData, err := os.ReadFile(firstArchive)
	if err != nil || string(oldData) != "old" {
		t.Fatalf("previous archive changed: data=%q err=%v", oldData, err)
	}
	newData, err := os.ReadFile(firstArchive + ".1")
	if err != nil || string(newData) != "new" {
		t.Fatalf("new archive missing: data=%q err=%v", newData, err)
	}
}

func BenchmarkDecodeLegacyNodes(b *testing.B) {
	const nodeCount = 20_000
	var content strings.Builder
	content.WriteString(`{"metadata":{"ignored":true},"nodes":[`)
	for index := range nodeCount {
		if index > 0 {
			content.WriteByte(',')
		}
		_, _ = fmt.Fprintf(
			&content,
			`{"raw_uri":"http://127.0.0.1:%d","type":"http","name":"node-%d"}`,
			10_000+index,
			index,
		)
	}
	content.WriteString(`]}`)
	payload := content.String()
	b.SetBytes(int64(len(payload)))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		visited := 0
		count, err := decodeLegacyNodes(strings.NewReader(payload), func(legacyNode) error {
			visited++
			return nil
		})
		if err != nil || count != nodeCount || visited != nodeCount {
			b.Fatalf("count=%d visited=%d err=%v", count, visited, err)
		}
	}
}

func openMigrationTestDB(t *testing.T) *sql.DB {
	t.Helper()
	database, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "migration.db"))
	if err != nil {
		t.Fatal(err)
	}
	database.SetMaxOpenConns(1)
	if err := database.Ping(); err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	if err := createTables(database); err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return database
}
