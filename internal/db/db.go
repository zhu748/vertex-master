package db

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
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

type legacyNode struct {
	Type     string `json:"type"`
	Name     string `json:"name"`
	RawURI   string `json:"raw_uri"`
	Disabled bool   `json:"disabled"`
}

type legacyNodeHealth struct { //nolint:govet
	SuccessCount        int     `json:"success_count"`
	FailCount           int     `json:"fail_count"`
	ConsecutiveFailures int     `json:"consecutive_failures"`
	LastTestMs          float64 `json:"last_test_ms"`
	LastTestError       string  `json:"last_test_error"`
	LastSuccessAt       int64   `json:"last_success_at"`
	LastFailAt          int64   `json:"last_fail_at"`
	CooldownUntil       int64   `json:"cooldown_until"`
}

func migrateFromFiles(db *sql.DB, configDir string) {
	migratedFolder := filepath.Join(configDir, "migrated")
	nodesPath := filepath.Join(configDir, "nodes.json")
	nodeCount, err := migrateLegacyNodesFile(db, nodesPath, migratedFolder)
	switch {
	case err == nil:
		log.Printf("[DB] Migrated %d nodes from nodes.json", nodeCount)
	case !os.IsNotExist(err):
		log.Printf("[DB] nodes.json migration failed; source retained: %v", err)
	}

	healthPath := filepath.Join(configDir, "node_health.json")
	healthCount, skipped, err := migrateLegacyHealthFile(db, healthPath, migratedFolder)
	switch {
	case err == nil:
		log.Printf("[DB] Migrated %d node health records from node_health.json", healthCount)
	case !os.IsNotExist(err):
		log.Printf(
			"[DB] node_health.json migration incomplete (migrated=%d skipped=%d); source retained: %v",
			healthCount,
			skipped,
			err,
		)
	}
}

func migrateLegacyNodesFile(db *sql.DB, path, migratedFolder string) (int, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	count, migrateErr := migrateLegacyNodes(db, file)
	closeErr := file.Close()
	if migrateErr != nil {
		return 0, fmt.Errorf("migrate %s: %w", path, migrateErr)
	}
	if closeErr != nil {
		return count, fmt.Errorf("close %s after migration: %w", path, closeErr)
	}
	if err := archiveLegacyFile(path, migratedFolder); err != nil {
		return count, err
	}
	return count, nil
}

func migrateLegacyHealthFile(db *sql.DB, path, migratedFolder string) (int, int, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, 0, err
	}
	migrated, skipped, migrateErr := migrateLegacyHealth(db, file)
	closeErr := file.Close()
	if migrateErr != nil {
		return 0, 0, fmt.Errorf("migrate %s: %w", path, migrateErr)
	}
	if closeErr != nil {
		return migrated, skipped, fmt.Errorf("close %s after migration: %w", path, closeErr)
	}
	if skipped > 0 {
		return migrated, skipped, fmt.Errorf(
			"%d health records reference nodes that were not migrated",
			skipped,
		)
	}
	if err := archiveLegacyFile(path, migratedFolder); err != nil {
		return migrated, 0, err
	}
	return migrated, 0, nil
}

func migrateLegacyNodes(db *sql.DB, reader io.Reader) (int, error) {
	tx, err := db.Begin()
	if err != nil {
		return 0, fmt.Errorf("begin nodes migration: %w", err)
	}
	stmt, err := tx.Prepare(
		"INSERT OR IGNORE INTO nodes (raw_uri, type, name, disabled) VALUES (?, ?, ?, ?)",
	)
	if err != nil {
		_ = tx.Rollback()
		return 0, fmt.Errorf("prepare nodes migration: %w", err)
	}

	insertedCount := 0
	_, decodeErr := decodeLegacyNodes(reader, func(node legacyNode) error {
		result, execErr := stmt.Exec(node.RawURI, node.Type, node.Name, node.Disabled)
		if execErr != nil {
			return execErr
		}
		inserted, rowsErr := result.RowsAffected()
		if rowsErr != nil {
			return rowsErr
		}
		insertedCount += int(inserted)
		return nil
	})
	closeErr := stmt.Close()
	if decodeErr != nil {
		_ = tx.Rollback()
		return 0, fmt.Errorf("decode or insert nodes: %w", decodeErr)
	}
	if closeErr != nil {
		_ = tx.Rollback()
		return 0, fmt.Errorf("close nodes statement: %w", closeErr)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit nodes migration: %w", err)
	}
	return insertedCount, nil
}

func migrateLegacyHealth(db *sql.DB, reader io.Reader) (int, int, error) {
	tx, err := db.Begin()
	if err != nil {
		return 0, 0, fmt.Errorf("begin node health migration: %w", err)
	}
	stmt, err := tx.Prepare(`INSERT OR REPLACE INTO node_health
		(raw_uri, success_count, fail_count, consecutive_failures, last_test_ms,
		 last_test_error, last_success_at, last_fail_at, cooldown_until)
		SELECT ?, ?, ?, ?, ?, ?, ?, ?, ?
		WHERE EXISTS (SELECT 1 FROM nodes WHERE raw_uri = ?)`)
	if err != nil {
		_ = tx.Rollback()
		return 0, 0, fmt.Errorf("prepare node health migration: %w", err)
	}

	migrated := 0
	skipped := 0
	decodeErr := decodeLegacyHealth(reader, func(uri string, health legacyNodeHealth) error {
		result, execErr := stmt.Exec(
			uri,
			health.SuccessCount,
			health.FailCount,
			health.ConsecutiveFailures,
			health.LastTestMs,
			health.LastTestError,
			health.LastSuccessAt,
			health.LastFailAt,
			health.CooldownUntil,
			uri,
		)
		if execErr != nil {
			return execErr
		}
		inserted, rowsErr := result.RowsAffected()
		if rowsErr != nil {
			return rowsErr
		}
		if inserted == 0 {
			skipped++
		} else {
			migrated++
		}
		return nil
	})
	closeErr := stmt.Close()
	if decodeErr != nil {
		_ = tx.Rollback()
		return 0, 0, fmt.Errorf("decode or insert node health: %w", decodeErr)
	}
	if closeErr != nil {
		_ = tx.Rollback()
		return 0, 0, fmt.Errorf("close node health statement: %w", closeErr)
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("commit node health migration: %w", err)
	}
	return migrated, skipped, nil
}

func decodeLegacyNodes(reader io.Reader, visit func(legacyNode) error) (int, error) {
	decoder := json.NewDecoder(bufio.NewReaderSize(reader, 64<<10))
	if err := expectJSONDelimiter(decoder, '{'); err != nil {
		return 0, err
	}
	foundNodes := false
	visited := 0
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return 0, err
		}
		key, ok := keyToken.(string)
		if !ok {
			return 0, fmt.Errorf("nodes object key is %T, want string", keyToken)
		}
		if key != "nodes" {
			if err := skipJSONValue(decoder); err != nil {
				return 0, fmt.Errorf("skip nodes.json field %q: %w", key, err)
			}
			continue
		}
		if foundNodes {
			return 0, fmt.Errorf("duplicate nodes field")
		}
		foundNodes = true
		if err := expectJSONDelimiter(decoder, '['); err != nil {
			return 0, fmt.Errorf("nodes field: %w", err)
		}
		for decoder.More() {
			var node legacyNode
			if err := decoder.Decode(&node); err != nil {
				return 0, err
			}
			if err := visit(node); err != nil {
				return 0, err
			}
			visited++
		}
		if err := expectJSONDelimiter(decoder, ']'); err != nil {
			return 0, err
		}
	}
	if err := expectJSONDelimiter(decoder, '}'); err != nil {
		return 0, err
	}
	if err := expectJSONEOF(decoder); err != nil {
		return 0, err
	}
	return visited, nil
}

func decodeLegacyHealth(reader io.Reader, visit func(string, legacyNodeHealth) error) error {
	decoder := json.NewDecoder(bufio.NewReaderSize(reader, 64<<10))
	if err := expectJSONDelimiter(decoder, '{'); err != nil {
		return err
	}
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return err
		}
		uri, ok := keyToken.(string)
		if !ok {
			return fmt.Errorf("node health object key is %T, want string", keyToken)
		}
		var health legacyNodeHealth
		if err := decoder.Decode(&health); err != nil {
			return err
		}
		if err := visit(uri, health); err != nil {
			return err
		}
	}
	if err := expectJSONDelimiter(decoder, '}'); err != nil {
		return err
	}
	return expectJSONEOF(decoder)
}

func expectJSONDelimiter(decoder *json.Decoder, expected json.Delim) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	if token != expected {
		return fmt.Errorf("unexpected JSON token %v, want %q", token, expected)
	}
	return nil
}

func expectJSONEOF(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err == io.EOF {
		return nil
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("unexpected trailing JSON token %v", token)
}

func skipJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, compound := token.(json.Delim)
	if !compound {
		return nil
	}
	switch delimiter {
	case '{':
		for decoder.More() {
			if _, err := decoder.Token(); err != nil {
				return err
			}
			if err := skipJSONValue(decoder); err != nil {
				return err
			}
		}
		return expectJSONDelimiter(decoder, '}')
	case '[':
		for decoder.More() {
			if err := skipJSONValue(decoder); err != nil {
				return err
			}
		}
		return expectJSONDelimiter(decoder, ']')
	default:
		return fmt.Errorf("unexpected closing JSON delimiter %q", delimiter)
	}
}

func archiveLegacyFile(path, migratedFolder string) error {
	if err := os.MkdirAll(migratedFolder, 0755); err != nil {
		return fmt.Errorf("create migrated folder %s: %w", migratedFolder, err)
	}
	baseTarget := filepath.Join(migratedFolder, filepath.Base(path)+".migrated")
	target := baseTarget
	for suffix := 1; ; suffix++ {
		_, err := os.Stat(target)
		if os.IsNotExist(err) {
			break
		}
		if err != nil {
			return fmt.Errorf("inspect legacy archive %s: %w", target, err)
		}
		if suffix > 10_000 {
			return fmt.Errorf("too many legacy archives for %s", path)
		}
		target = fmt.Sprintf("%s.%d", baseTarget, suffix)
	}
	if err := os.Rename(path, target); err != nil {
		return fmt.Errorf("archive legacy file %s to %s: %w", path, target, err)
	}
	return nil
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
