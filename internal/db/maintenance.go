package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
)

var ErrNotInitialized = errors.New("database is not initialized") //nolint:gochecknoglobals // Sentinel error.

// Maintenance borrows the existing connection pool: no competing writer or
// second database is opened while health persistence is active.
type Maintenance struct{ database *sql.DB }

func NewMaintenance(database *sql.DB) *Maintenance { return &Maintenance{database: database} }

type DatabaseStats struct {
	FileBytes        int64 `json:"file_bytes"`
	WALBytes         int64 `json:"wal_bytes"`
	PageSize         int64 `json:"page_size"`
	PageCount        int64 `json:"page_count"`
	FreePages        int64 `json:"free_pages"`
	ReclaimableBytes int64 `json:"reclaimable_bytes"`
}

func (m *Maintenance) Stats(ctx context.Context) (DatabaseStats, error) {
	var out DatabaseStats
	if m.database == nil {
		return out, ErrNotInitialized
	}
	for _, item := range []struct {
		query  string
		target *int64
	}{
		{"PRAGMA page_size", &out.PageSize}, {"PRAGMA page_count", &out.PageCount}, {"PRAGMA freelist_count", &out.FreePages},
	} {
		if err := m.database.QueryRowContext(ctx, item.query).Scan(item.target); err != nil {
			return out, fmt.Errorf("database statistics: %w", err)
		}
	}
	out.ReclaimableBytes = out.FreePages * out.PageSize
	rows, err := m.database.QueryContext(ctx, "PRAGMA database_list")
	if err != nil {
		return out, err
	}
	var path string
	for rows.Next() {
		var seq int
		var name, file string
		if err := rows.Scan(&seq, &name, &file); err != nil {
			_ = rows.Close()
			return out, err
		}
		if name == "main" {
			path = file
		}
	}
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil {
		return out, err
	}
	for _, item := range []struct {
		suffix string
		target *int64
	}{{"", &out.FileBytes}, {"-wal", &out.WALBytes}} {
		if path == "" {
			continue
		}
		info, err := os.Stat(path + item.suffix)
		if err == nil {
			*item.target = info.Size()
		} else if !os.IsNotExist(err) {
			return out, err
		}
	}
	return out, nil
}

func (m *Maintenance) Check(ctx context.Context) ([]string, error) {
	if m.database == nil {
		return nil, ErrNotInitialized
	}
	rows, err := m.database.QueryContext(ctx, "PRAGMA quick_check(20)")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var results []string
	for rows.Next() {
		var result string
		if err := rows.Scan(&result); err != nil {
			return nil, err
		}
		results = append(results, result)
	}
	return results, rows.Err()
}

type CheckpointStats struct {
	Busy              int `json:"busy"`
	WALPages          int `json:"wal_pages"`
	CheckpointedPages int `json:"checkpointed_pages"`
}

// PASSIVE never forces readers out or waits for their transactions to finish.
func (m *Maintenance) Checkpoint(ctx context.Context) (CheckpointStats, error) {
	var out CheckpointStats
	if m.database == nil {
		return out, ErrNotInitialized
	}
	err := m.database.QueryRowContext(ctx, "PRAGMA wal_checkpoint(PASSIVE)").Scan(&out.Busy, &out.WALPages, &out.CheckpointedPages)
	return out, err
}
