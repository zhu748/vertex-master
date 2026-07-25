package nodes

import (
	"errors"
	"fmt"
	"time"

	"github.com/bsfdsagfadg/vertex/internal/db"
)

type ProxySubscription struct {
	ID                     int64  `json:"id"`
	Name                   string `json:"name"`
	URL                    string `json:"url"`
	ProxyType              string `json:"proxy_type"`
	RefreshIntervalMinutes int    `json:"refresh_interval_minutes"`
	Enabled                bool   `json:"enabled"`
	LastRefreshedAt        int64  `json:"last_refreshed_at"`
	LastError              string `json:"last_error"`
	NodeCount              int    `json:"node_count"`
	CreatedAt              int64  `json:"created_at"`
	UpdatedAt              int64  `json:"updated_at"`
}

func ListProxySubscriptions() ([]ProxySubscription, error) {
	if db.GlobalDB == nil {
		return nil, errors.New("database unavailable")
	}
	rows, err := db.GlobalDB.Query(`SELECT id, name, url, proxy_type, refresh_interval_minutes,
		enabled, last_refreshed_at, last_error, node_count, created_at, updated_at
		FROM proxy_subscriptions ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list proxy subscriptions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []ProxySubscription{}
	for rows.Next() {
		var item ProxySubscription
		if err := rows.Scan(
			&item.ID, &item.Name, &item.URL, &item.ProxyType, &item.RefreshIntervalMinutes,
			&item.Enabled, &item.LastRefreshedAt, &item.LastError, &item.NodeCount,
			&item.CreatedAt, &item.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan proxy subscription: %w", err)
		}
		out = append(out, item)
	}
	return out, rows.Err() //nolint:wrapcheck
}

func GetProxySubscription(id int64) (ProxySubscription, error) {
	if db.GlobalDB == nil {
		return ProxySubscription{}, errors.New("database unavailable")
	}
	var item ProxySubscription
	err := db.GlobalDB.QueryRow(`SELECT id, name, url, proxy_type, refresh_interval_minutes,
		enabled, last_refreshed_at, last_error, node_count, created_at, updated_at
		FROM proxy_subscriptions WHERE id = ?`, id).Scan(
		&item.ID, &item.Name, &item.URL, &item.ProxyType, &item.RefreshIntervalMinutes,
		&item.Enabled, &item.LastRefreshedAt, &item.LastError, &item.NodeCount,
		&item.CreatedAt, &item.UpdatedAt,
	)
	if err != nil {
		return ProxySubscription{}, fmt.Errorf("get proxy subscription: %w", err)
	}
	return item, nil
}

func SaveProxySubscription(item ProxySubscription) (ProxySubscription, error) {
	if db.GlobalDB == nil {
		return ProxySubscription{}, errors.New("database unavailable")
	}
	now := time.Now().Unix()
	if item.ID == 0 {
		result, err := db.GlobalDB.Exec(`INSERT INTO proxy_subscriptions
			(name, url, proxy_type, refresh_interval_minutes, enabled, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			item.Name, item.URL, item.ProxyType, item.RefreshIntervalMinutes, item.Enabled, now, now)
		if err != nil {
			return ProxySubscription{}, fmt.Errorf("insert proxy subscription: %w", err)
		}
		item.ID, _ = result.LastInsertId()
	} else {
		result, err := db.GlobalDB.Exec(`UPDATE proxy_subscriptions SET
			name = ?, url = ?, proxy_type = ?, refresh_interval_minutes = ?, enabled = ?, updated_at = ?
			WHERE id = ?`,
			item.Name, item.URL, item.ProxyType, item.RefreshIntervalMinutes, item.Enabled, now, item.ID)
		if err != nil {
			return ProxySubscription{}, fmt.Errorf("update proxy subscription: %w", err)
		}
		if affected, _ := result.RowsAffected(); affected == 0 {
			return ProxySubscription{}, errors.New("proxy subscription not found")
		}
	}
	return GetProxySubscription(item.ID)
}

func UpdateProxySubscriptionResult(id int64, nodeCount int, refreshErr error) error {
	if db.GlobalDB == nil {
		return errors.New("database unavailable")
	}
	lastError := ""
	if refreshErr != nil {
		lastError = refreshErr.Error()
	}
	_, err := db.GlobalDB.Exec(`UPDATE proxy_subscriptions SET
		last_refreshed_at = ?, last_error = ?, node_count = ?, updated_at = ?
		WHERE id = ?`, time.Now().Unix(), lastError, nodeCount, time.Now().Unix(), id)
	if err != nil {
		return fmt.Errorf("update proxy subscription result: %w", err)
	}
	return nil
}

func DeleteProxySubscription(id int64) error {
	if db.GlobalDB == nil {
		return errors.New("database unavailable")
	}
	result, err := db.GlobalDB.Exec("DELETE FROM proxy_subscriptions WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("delete proxy subscription: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return errors.New("proxy subscription not found")
	}
	return nil
}

func DueProxySubscriptions(now time.Time) ([]ProxySubscription, error) {
	items, err := ListProxySubscriptions()
	if err != nil {
		return nil, err
	}
	due := make([]ProxySubscription, 0, len(items))
	for _, item := range items {
		if !item.Enabled {
			continue
		}
		interval := time.Duration(item.RefreshIntervalMinutes) * time.Minute
		if item.LastRefreshedAt == 0 || now.Sub(time.Unix(item.LastRefreshedAt, 0)) >= interval {
			due = append(due, item)
		}
	}
	return due, nil
}
