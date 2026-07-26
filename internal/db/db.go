package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"

	_ "github.com/glebarez/go-sqlite"
)

var (
	GlobalDB *sql.DB    //nolint:gochecknoglobals
	mu       sync.Mutex //nolint:gochecknoglobals
)

// InitDB initializes the SQLite database at the given path.
// If it's a new database, it attempts to migrate data from nodes.json and node_health.json.
func InitDB(dbPath string) error {
	mu.Lock()
	defer mu.Unlock()

	if GlobalDB != nil {
		return nil // Already initialized
	}

	dir := filepath.Dir(dbPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("创建数据库目录 %s: %w", dir, err)
	}

	isNewDB := false
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		isNewDB = true
	}

	// Use WAL mode for better concurrency
	dsn := dbPath +
		"?_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=foreign_keys(1)" +
		"&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return fmt.Errorf("打开 SQLite 数据库 %s: %w", dbPath, err)
	}
	// SQLite permits many readers but only one writer. Keeping one pooled
	// connection serializes background health writes with admin mutations and
	// avoids SQLITE_BUSY errors from competing connections in this process.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	// Ensure DB is reachable
	if errPing := db.Ping(); errPing != nil {
		_ = db.Close()
		return fmt.Errorf("连接 SQLite 数据库 %s: %w", dbPath, errPing)
	}

	// Create tables
	err = createTables(db)
	if err != nil {
		_ = db.Close()
		return err
	}

	// Migrate if new
	if isNewDB {
		log.Printf("[DB] New database created at %s, attempting to migrate from legacy files...", dbPath)
		migrateFromFiles(db, dir)
	}

	GlobalDB = db
	return nil
}

// CurrentDB returns a concurrency-safe snapshot of the active database handle.
// sql.DB itself supports concurrent use; the lock only protects replacing the global pointer.
func CurrentDB() *sql.DB {
	mu.Lock()
	defer mu.Unlock()
	return GlobalDB
}

func createTables(db *sql.DB) error {
	schema := `
	CREATE TABLE IF NOT EXISTS nodes (
		raw_uri TEXT PRIMARY KEY,
		type TEXT NOT NULL,
		name TEXT NOT NULL,
		disabled BOOLEAN NOT NULL DEFAULT 0,
		source_id INTEGER NOT NULL DEFAULT 0,
		sort_order INTEGER NOT NULL DEFAULT 0
	);

	CREATE TABLE IF NOT EXISTS node_health (
		raw_uri TEXT PRIMARY KEY,
		success_count INTEGER NOT NULL DEFAULT 0,
		fail_count INTEGER NOT NULL DEFAULT 0,
		consecutive_failures INTEGER NOT NULL DEFAULT 0,
		last_test_ms REAL NOT NULL DEFAULT 0,
		last_test_error TEXT NOT NULL DEFAULT '',
		last_success_at INTEGER NOT NULL DEFAULT 0,
		last_fail_at INTEGER NOT NULL DEFAULT 0,
		cooldown_until INTEGER NOT NULL DEFAULT 0,
		last_429_at INTEGER NOT NULL DEFAULT 0,
		rate_limit_count INTEGER NOT NULL DEFAULT 0,
		recent_use_count INTEGER NOT NULL DEFAULT 0,
		last_selected_at INTEGER NOT NULL DEFAULT 0,
		FOREIGN KEY(raw_uri) REFERENCES nodes(raw_uri) ON DELETE CASCADE
	);

	CREATE TABLE IF NOT EXISTS proxy_subscriptions (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		managed_key TEXT NOT NULL DEFAULT '',
		name TEXT NOT NULL,
		url TEXT NOT NULL,
		proxy_type TEXT NOT NULL DEFAULT 'auto',
		refresh_interval_minutes INTEGER NOT NULL DEFAULT 60,
		enabled BOOLEAN NOT NULL DEFAULT 1,
		last_refreshed_at INTEGER NOT NULL DEFAULT 0,
		last_attempt_at INTEGER NOT NULL DEFAULT 0,
		last_error TEXT NOT NULL DEFAULT '',
		consecutive_failures INTEGER NOT NULL DEFAULT 0,
		node_count INTEGER NOT NULL DEFAULT 0,
		created_at INTEGER NOT NULL DEFAULT 0,
		updated_at INTEGER NOT NULL DEFAULT 0
	);

	CREATE TABLE IF NOT EXISTS proxy_subscription_nodes (
		subscription_id INTEGER NOT NULL,
		raw_uri TEXT NOT NULL,
		PRIMARY KEY(subscription_id, raw_uri),
		FOREIGN KEY(subscription_id) REFERENCES proxy_subscriptions(id) ON DELETE CASCADE,
		FOREIGN KEY(raw_uri) REFERENCES nodes(raw_uri) ON DELETE CASCADE
	);

	CREATE INDEX IF NOT EXISTS idx_proxy_subscription_nodes_raw_uri
		ON proxy_subscription_nodes(raw_uri);
	`
	_, err := db.Exec(schema)
	if err != nil {
		return fmt.Errorf("创建数据库表结构: %w", err)
	}

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin database migration: %w", err)
	}
	rollback := func(migrationErr error) error {
		_ = tx.Rollback()
		return migrationErr
	}

	columns := []struct {
		table      string
		name       string
		definition string
	}{
		{"nodes", "source_id", "INTEGER NOT NULL DEFAULT 0"},
		{"nodes", "sort_order", "INTEGER NOT NULL DEFAULT 0"},
		{"proxy_subscriptions", "last_attempt_at", "INTEGER NOT NULL DEFAULT 0"},
		{"proxy_subscriptions", "consecutive_failures", "INTEGER NOT NULL DEFAULT 0"},
		{"proxy_subscriptions", "managed_key", "TEXT NOT NULL DEFAULT ''"},
		{"node_health", "last_429_at", "INTEGER NOT NULL DEFAULT 0"},
		{"node_health", "rate_limit_count", "INTEGER NOT NULL DEFAULT 0"},
		{"node_health", "recent_use_count", "INTEGER NOT NULL DEFAULT 0"},
		{"node_health", "last_selected_at", "INTEGER NOT NULL DEFAULT 0"},
	}
	for _, column := range columns {
		if err := ensureColumn(tx, column.table, column.name, column.definition); err != nil {
			return rollback(err)
		}
	}
	if _, err := tx.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_proxy_subscriptions_managed_key
		ON proxy_subscriptions(managed_key) WHERE managed_key <> ''`); err != nil {
		return rollback(fmt.Errorf("create managed subscription index: %w", err))
	}
	if _, err := tx.Exec(`INSERT OR IGNORE INTO proxy_subscription_nodes(subscription_id, raw_uri)
		SELECT n.source_id, n.raw_uri
		FROM nodes n
		JOIN proxy_subscriptions s ON s.id = n.source_id
		WHERE n.source_id > 0`); err != nil {
		return rollback(fmt.Errorf("backfill proxy subscription nodes: %w", err))
	}
	if _, err := tx.Exec(`DELETE FROM proxy_subscription_nodes
		WHERE NOT EXISTS (
			SELECT 1 FROM proxy_subscriptions s
			WHERE s.id = proxy_subscription_nodes.subscription_id
		)
		OR NOT EXISTS (
			SELECT 1 FROM nodes n
			WHERE n.raw_uri = proxy_subscription_nodes.raw_uri
		)`); err != nil {
		return rollback(fmt.Errorf("remove orphan proxy subscription relations: %w", err))
	}
	if _, err := tx.Exec(`DELETE FROM nodes
		WHERE source_id > 0
		AND NOT EXISTS (
			SELECT 1 FROM proxy_subscription_nodes psn WHERE psn.raw_uri = nodes.raw_uri
		)`); err != nil {
		return rollback(fmt.Errorf("remove orphan subscription nodes: %w", err))
	}
	if _, err := tx.Exec(`DELETE FROM node_health
		WHERE NOT EXISTS (
			SELECT 1 FROM nodes n WHERE n.raw_uri = node_health.raw_uri
		)`); err != nil {
		return rollback(fmt.Errorf("remove orphan node health: %w", err))
	}
	if err := validateForeignKeys(tx); err != nil {
		return rollback(err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit database migration: %w", err)
	}
	return nil
}

func ensureColumn(tx *sql.Tx, table, column, definition string) error {
	rows, err := tx.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return fmt.Errorf("inspect %s columns: %w", table, err)
	}
	found := false
	for rows.Next() {
		var (
			cid       int
			name      string
			columnTyp string
			notNull   int
			defaultV  any
			primary   int
		)
		if err := rows.Scan(&cid, &name, &columnTyp, &notNull, &defaultV, &primary); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan %s columns: %w", table, err)
		}
		if name == column {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("iterate %s columns: %w", table, err)
	}
	_ = rows.Close()
	if found {
		return nil
	}
	if _, err := tx.Exec(
		"ALTER TABLE " + table + " ADD COLUMN " + column + " " + definition,
	); err != nil {
		return fmt.Errorf("add %s.%s: %w", table, column, err)
	}
	return nil
}

func validateForeignKeys(tx *sql.Tx) error {
	rows, err := tx.Query("PRAGMA foreign_key_check")
	if err != nil {
		return fmt.Errorf("check foreign keys: %w", err)
	}
	defer func() { _ = rows.Close() }()
	if rows.Next() {
		var table string
		var rowID int64
		var parent string
		var foreignKeyID int
		if err := rows.Scan(&table, &rowID, &parent, &foreignKeyID); err != nil {
			return fmt.Errorf("scan foreign key violation: %w", err)
		}
		return fmt.Errorf(
			"foreign key violation in table %s row %d referencing %s (constraint %d)",
			table,
			rowID,
			parent,
			foreignKeyID,
		)
	}
	return rows.Err() //nolint:wrapcheck
}
func migrateFromFiles(db *sql.DB, configDir string) {
	migratedFolder := filepath.Join(configDir, "migrated")

	// Migrate nodes
	nodesPath := filepath.Join(configDir, "nodes.json")
	if data, err := os.ReadFile(nodesPath); err == nil {
		var d struct {
			Nodes []struct {
				Type     string `json:"type"`
				Name     string `json:"name"`
				RawURI   string `json:"raw_uri"`
				Disabled bool   `json:"disabled"`
			} `json:"nodes"`
		}
		if errUnm := json.Unmarshal(data, &d); errUnm == nil { //nolint:govet
			tx, _ := db.Begin()
			stmt, _ := tx.Prepare("INSERT OR IGNORE INTO nodes (raw_uri, type, name, disabled) VALUES (?, ?, ?, ?)")
			for _, n := range d.Nodes {
				_, _ = stmt.Exec(n.RawURI, n.Type, n.Name, n.Disabled)
			}
			_ = stmt.Close()
			_ = tx.Commit()
			log.Printf("[DB] Migrated %d nodes from nodes.json", len(d.Nodes))

			_ = os.MkdirAll(migratedFolder, 0755)
			_ = os.Rename(nodesPath, filepath.Join(migratedFolder, "nodes.json.migrated"))
		}
	}

	// Migrate node_health
	healthPath := filepath.Join(configDir, "node_health.json")
	if data, err := os.ReadFile(healthPath); err == nil {
		var healthMap map[string]struct { //nolint:govet
			SuccessCount        int     `json:"success_count"`
			FailCount           int     `json:"fail_count"`
			ConsecutiveFailures int     `json:"consecutive_failures"`
			LastTestMs          float64 `json:"last_test_ms"`
			LastTestError       string  `json:"last_test_error"`
			LastSuccessAt       int64   `json:"last_success_at"`
			LastFailAt          int64   `json:"last_fail_at"`
			CooldownUntil       int64   `json:"cooldown_until"`
		}
		if errUnm := json.Unmarshal(data, &healthMap); errUnm == nil { //nolint:govet
			tx, _ := db.Begin()
			stmt, _ := tx.Prepare(`INSERT OR REPLACE INTO node_health 
				(raw_uri, success_count, fail_count, consecutive_failures, last_test_ms, last_test_error, last_success_at, last_fail_at, cooldown_until) 
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`)
			migrated := 0
			for uri, h := range healthMap {
				_, err := stmt.Exec(uri, h.SuccessCount, h.FailCount, h.ConsecutiveFailures, h.LastTestMs, h.LastTestError, h.LastSuccessAt, h.LastFailAt, h.CooldownUntil) //nolint:govet
				if err == nil {
					migrated++
				}
			}
			_ = stmt.Close()
			_ = tx.Commit()
			log.Printf("[DB] Migrated %d node health records from node_health.json", migrated)

			_ = os.MkdirAll(migratedFolder, 0755)
			_ = os.Rename(healthPath, filepath.Join(migratedFolder, "node_health.json.migrated"))
		}
	}
}

// CloseDB closes the global database connection.
func CloseDB() {
	mu.Lock()
	defer mu.Unlock()
	if GlobalDB != nil {
		_ = GlobalDB.Close()
		GlobalDB = nil
	}
}
