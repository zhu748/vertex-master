package nodes

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bsfdsagfadg/vertex/internal/db"
)

type ProxySubscription struct {
	ID                     int64  `json:"id"`
	ManagedKey             string `json:"managed_key,omitempty"`
	Name                   string `json:"name"`
	URL                    string `json:"url"`
	ProxyType              string `json:"proxy_type"`
	RefreshIntervalMinutes int    `json:"refresh_interval_minutes"`
	Enabled                bool   `json:"enabled"`
	LastRefreshedAt        int64  `json:"last_refreshed_at"`
	LastAttemptAt          int64  `json:"last_attempt_at"`
	LastError              string `json:"last_error"`
	ConsecutiveFailures    int    `json:"consecutive_failures"`
	NodeCount              int    `json:"node_count"`
	CreatedAt              int64  `json:"created_at"`
	UpdatedAt              int64  `json:"updated_at"`
}

func ListProxySubscriptions() ([]ProxySubscription, error) {
	database := db.CurrentDB()
	if database == nil {
		return nil, errors.New("database unavailable")
	}
	rows, err := database.Query(`SELECT id, managed_key, name, url, proxy_type, refresh_interval_minutes,
		enabled, last_refreshed_at, last_attempt_at, last_error, consecutive_failures,
		node_count, created_at, updated_at
		FROM proxy_subscriptions ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list proxy subscriptions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []ProxySubscription{}
	for rows.Next() {
		var item ProxySubscription
		if err := rows.Scan(
			&item.ID, &item.ManagedKey, &item.Name, &item.URL, &item.ProxyType, &item.RefreshIntervalMinutes,
			&item.Enabled, &item.LastRefreshedAt, &item.LastAttemptAt, &item.LastError,
			&item.ConsecutiveFailures, &item.NodeCount,
			&item.CreatedAt, &item.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan proxy subscription: %w", err)
		}
		out = append(out, item)
	}
	return out, rows.Err() //nolint:wrapcheck
}

func GetProxySubscription(id int64) (ProxySubscription, error) {
	database := db.CurrentDB()
	if database == nil {
		return ProxySubscription{}, errors.New("database unavailable")
	}
	var item ProxySubscription
	err := database.QueryRow(`SELECT id, managed_key, name, url, proxy_type, refresh_interval_minutes,
		enabled, last_refreshed_at, last_attempt_at, last_error, consecutive_failures,
		node_count, created_at, updated_at
		FROM proxy_subscriptions WHERE id = ?`, id).Scan(
		&item.ID, &item.ManagedKey, &item.Name, &item.URL, &item.ProxyType, &item.RefreshIntervalMinutes,
		&item.Enabled, &item.LastRefreshedAt, &item.LastAttemptAt, &item.LastError,
		&item.ConsecutiveFailures, &item.NodeCount,
		&item.CreatedAt, &item.UpdatedAt,
	)
	if err != nil {
		return ProxySubscription{}, fmt.Errorf("get proxy subscription: %w", err)
	}
	return item, nil
}

func SaveProxySubscription(item ProxySubscription) (ProxySubscription, error) {
	database := db.CurrentDB()
	if database == nil {
		return ProxySubscription{}, errors.New("database unavailable")
	}
	now := time.Now().Unix()
	if item.ID == 0 {
		result, err := database.Exec(`INSERT INTO proxy_subscriptions
			(managed_key, name, url, proxy_type, refresh_interval_minutes, enabled, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			item.ManagedKey, item.Name, item.URL, item.ProxyType, item.RefreshIntervalMinutes, item.Enabled, now, now)
		if err != nil {
			return ProxySubscription{}, fmt.Errorf("insert proxy subscription: %w", err)
		}
		item.ID, _ = result.LastInsertId()
	} else {
		result, err := database.Exec(`UPDATE proxy_subscriptions SET
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

func GetManagedProxySubscription(managedKey string) (ProxySubscription, error) {
	database := db.CurrentDB()
	if database == nil {
		return ProxySubscription{}, errors.New("database unavailable")
	}
	var item ProxySubscription
	err := database.QueryRow(`SELECT id, managed_key, name, url, proxy_type, refresh_interval_minutes,
		enabled, last_refreshed_at, last_attempt_at, last_error, consecutive_failures,
		node_count, created_at, updated_at
		FROM proxy_subscriptions WHERE managed_key = ?`, managedKey).Scan(
		&item.ID, &item.ManagedKey, &item.Name, &item.URL, &item.ProxyType, &item.RefreshIntervalMinutes,
		&item.Enabled, &item.LastRefreshedAt, &item.LastAttemptAt, &item.LastError,
		&item.ConsecutiveFailures, &item.NodeCount,
		&item.CreatedAt, &item.UpdatedAt,
	)
	if err != nil {
		return ProxySubscription{}, fmt.Errorf("get managed proxy subscription: %w", err)
	}
	return item, nil
}

func UpsertManagedProxySubscription(managedKey string, item ProxySubscription) (ProxySubscription, error) {
	managedKey = strings.TrimSpace(managedKey)
	if managedKey == "" {
		return ProxySubscription{}, errors.New("managed proxy subscription key is required")
	}
	current, err := GetManagedProxySubscription(managedKey)
	if err == nil {
		item.ID = current.ID
		item.ManagedKey = managedKey
		return SaveProxySubscription(item)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return ProxySubscription{}, err
	}
	item.ID = 0
	item.ManagedKey = managedKey
	return SaveProxySubscription(item)
}

func UpdateProxySubscriptionResult(id int64, nodeCount int, refreshErr error) error {
	database := db.CurrentDB()
	if database == nil {
		return errors.New("database unavailable")
	}
	now := time.Now().Unix()
	if refreshErr != nil {
		_, err := database.Exec(`UPDATE proxy_subscriptions SET
			last_attempt_at = ?, last_error = ?, consecutive_failures = consecutive_failures + 1,
			updated_at = ? WHERE id = ?`,
			now, refreshErr.Error(), now, id)
		if err != nil {
			return fmt.Errorf("update failed proxy subscription result: %w", err)
		}
		return nil
	}
	_, err := database.Exec(`UPDATE proxy_subscriptions SET
		last_refreshed_at = ?, last_attempt_at = ?, last_error = '',
		consecutive_failures = 0, node_count = ?, updated_at = ?
		WHERE id = ?`, now, now, nodeCount, now, id)
	if err != nil {
		return fmt.Errorf("update proxy subscription result: %w", err)
	}
	return nil
}

func DeleteProxySubscription(id int64) error {
	database := db.CurrentDB()
	if database == nil {
		return errors.New("database unavailable")
	}
	result, err := database.Exec("DELETE FROM proxy_subscriptions WHERE id = ?", id)
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
		if item.LastError != "" && item.LastAttemptAt > 0 {
			retryDelay := proxySubscriptionRetryDelay(item.ConsecutiveFailures, interval)
			if now.Sub(time.Unix(item.LastAttemptAt, 0)) >= retryDelay {
				due = append(due, item)
			}
			continue
		}
		if item.LastRefreshedAt == 0 || now.Sub(time.Unix(item.LastRefreshedAt, 0)) >= interval {
			due = append(due, item)
		}
	}
	return due, nil
}

func proxySubscriptionRetryDelay(failures int, interval time.Duration) time.Duration {
	if failures < 1 {
		failures = 1
	}
	shift := min(failures-1, 4)
	delay := time.Minute * time.Duration(1<<shift)
	if delay > 15*time.Minute {
		delay = 15 * time.Minute
	}
	if interval > 0 && delay > interval {
		return interval
	}
	return delay
}
