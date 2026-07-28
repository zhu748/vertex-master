package nodes

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"math/rand"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/bsfdsagfadg/vertex/internal/db"
)

type Node struct {
	Type                    string `json:"type"`
	Name                    string `json:"name"`
	RawURI                  string `json:"raw_uri"`
	Disabled                bool   `json:"disabled"`
	SourceID                int64  `json:"source_id,omitempty"`
	SubscriptionSourceCount int    `json:"subscription_source_count,omitempty"`
}

type NodeHealth struct { //nolint:govet
	SuccessCount        int     `json:"success_count"`
	FailCount           int     `json:"fail_count"`
	ConsecutiveFailures int     `json:"consecutive_failures"`
	LastTestMs          float64 `json:"last_test_ms"`
	LastTestError       string  `json:"last_test_error"`
	LastSuccessAt       int64   `json:"last_success_at"`
	LastFailAt          int64   `json:"last_fail_at"`
	CooldownUntil       int64   `json:"cooldown_until"`
	Last429At           int64   `json:"last_429_at"`
	RateLimitCount      int     `json:"rate_limit_count"`
	RecentUseCount      int     `json:"recent_use_count"`
	LastSelectedAt      int64   `json:"last_selected_at"`
}

var (
	mu                  sync.RWMutex                      //nolint:gochecknoglobals
	nodeList            []Node                            //nolint:gochecknoglobals
	nodeIndexByURI      = make(map[string]int)            //nolint:gochecknoglobals
	healthMap           = make(map[string]*NodeHealth)    //nolint:gochecknoglobals
	subscriptionSources = make(map[string]map[int64]bool) //nolint:gochecknoglobals
	loaded              bool                              //nolint:gochecknoglobals
	DeleteNodeCallback  func(uri string)                  //nolint:gochecknoglobals
)

func ensureLoaded() {
	if loaded {
		return
	}

	database := db.CurrentDB()
	if database == nil {
		return
	}

	// Load nodes
	rows, err := database.Query(`SELECT raw_uri, type, name, disabled, source_id
		FROM nodes ORDER BY sort_order, rowid`)
	if err != nil {
		log.Printf("[Nodes] 加载节点失败: %v", err)
		return
	}
	loadedNodes := []Node{}
	for rows.Next() {
		var n Node
		if err := rows.Scan(&n.RawURI, &n.Type, &n.Name, &n.Disabled, &n.SourceID); err != nil {
			_ = rows.Close()
			log.Printf("[Nodes] 读取节点失败: %v", err)
			return
		}
		loadedNodes = append(loadedNodes, n)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		log.Printf("[Nodes] 遍历节点失败: %v", err)
		return
	}
	_ = rows.Close()

	// Load health
	hRows, err := database.Query(`SELECT raw_uri, success_count, fail_count, consecutive_failures,
		last_test_ms, last_test_error, last_success_at, last_fail_at, cooldown_until,
		last_429_at, rate_limit_count, recent_use_count, last_selected_at
		FROM node_health`)
	if err != nil {
		log.Printf("[Nodes] 加载健康记录失败: %v", err)
		return
	}
	loadedHealth := make(map[string]*NodeHealth)
	for hRows.Next() {
		var uri string
		h := &NodeHealth{} //nolint:exhaustruct
		if err := hRows.Scan(
			&uri, &h.SuccessCount, &h.FailCount, &h.ConsecutiveFailures,
			&h.LastTestMs, &h.LastTestError, &h.LastSuccessAt, &h.LastFailAt,
			&h.CooldownUntil, &h.Last429At, &h.RateLimitCount,
			&h.RecentUseCount, &h.LastSelectedAt,
		); err != nil {
			_ = hRows.Close()
			log.Printf("[Nodes] 读取健康记录失败: %v", err)
			return
		}
		loadedHealth[uri] = h
	}
	if err := hRows.Err(); err != nil {
		_ = hRows.Close()
		log.Printf("[Nodes] 遍历健康记录失败: %v", err)
		return
	}
	_ = hRows.Close()

	sourceRows, err := database.Query("SELECT subscription_id, raw_uri FROM proxy_subscription_nodes")
	if err != nil {
		log.Printf("[Nodes] 加载订阅归属失败: %v", err)
		return
	}
	loadedSources := make(map[string]map[int64]bool)
	for sourceRows.Next() {
		var sourceID int64
		var rawURI string
		if err := sourceRows.Scan(&sourceID, &rawURI); err != nil {
			_ = sourceRows.Close()
			log.Printf("[Nodes] 读取订阅归属失败: %v", err)
			return
		}
		if loadedSources[rawURI] == nil {
			loadedSources[rawURI] = make(map[int64]bool)
		}
		loadedSources[rawURI][sourceID] = true
	}
	if err := sourceRows.Err(); err != nil {
		_ = sourceRows.Close()
		log.Printf("[Nodes] 遍历订阅归属失败: %v", err)
		return
	}
	_ = sourceRows.Close()

	nodeList = loadedNodes
	rebuildNodeIndexUnsafe()
	healthMap = loadedHealth
	subscriptionSources = loadedSources
	loaded = true
	pruneHealthUnsafe()
	for uri, health := range healthMap {
		if decayHealthCounters(health) {
			updateSingleNodeHealthUnsafe(uri, health)
		}
	}
}

// lockLoadedForRead 返回时持有 mu.RLock。首次访问需要加载数据库时先临时
// 升级为写锁；调用方完成只读快照后必须 RUnlock。
func lockLoadedForRead() {
	mu.RLock()
	if loaded {
		return
	}
	mu.RUnlock()

	mu.Lock()
	ensureLoaded()
	mu.Unlock()
	mu.RLock()
}

func rebuildNodeIndexUnsafe() {
	index := make(map[string]int, len(nodeList))
	for position := range nodeList {
		index[nodeList[position].RawURI] = position
	}
	nodeIndexByURI = index
}

func lookupNodeUnsafe(uri string) (Node, bool) {
	if position, ok := nodeIndexByURI[uri]; ok &&
		position >= 0 && position < len(nodeList) && nodeList[position].RawURI == uri {
		return nodeList[position], true
	}
	for _, node := range nodeList {
		if node.RawURI == uri {
			return node, true
		}
	}
	return Node{}, false
}

func containsNodeForUpdateUnsafe(uri string) bool {
	if position, ok := nodeIndexByURI[uri]; ok &&
		position >= 0 && position < len(nodeList) && nodeList[position].RawURI == uri {
		return true
	}
	// 列表增删/排序后索引可能暂时陈旧；写路径发现 miss 时重建一次，
	// 后续候选启动与名称查询恢复 O(1)。
	rebuildNodeIndexUnsafe()
	_, ok := nodeIndexByURI[uri]
	return ok
}

func LoadNodes() []Node {
	lockLoadedForRead()
	defer mu.RUnlock()
	log.Printf("[Nodes] 获取所有节点 (数量: %d)", len(nodeList))
	out := append([]Node(nil), nodeList...)
	for i := range out {
		out[i].SubscriptionSourceCount = len(subscriptionSources[out[i].RawURI])
	}
	return out
}

func LoadHealth() map[string]*NodeHealth {
	lockLoadedForRead()
	defer mu.RUnlock()
	out := make(map[string]*NodeHealth, len(healthMap))
	for uri, health := range healthMap {
		if health == nil {
			out[uri] = nil
			continue
		}
		copied := *health
		out[uri] = &copied
	}
	return out
}

// writeAtomicJSON has been removed because it is unused

func saveNodesUnsafe() error {
	return saveNodesUnsafeWithTx(nil)
}

func saveNodesUnsafeWithTx(finalize func(*sql.Tx) error) error {
	database := db.CurrentDB()
	if database == nil {
		return nil
	}
	tx, err := database.Begin()
	if err != nil {
		return fmt.Errorf("开始保存节点事务: %w", err)
	}
	rollback := func(saveErr error) error {
		_ = tx.Rollback()
		return fmt.Errorf("保存节点事务: %w", saveErr)
	}

	type persistedNode struct {
		node      Node
		sortOrder int
	}
	existingRows, err := tx.Query(
		"SELECT raw_uri, type, name, disabled, source_id, sort_order FROM nodes",
	)
	if err != nil {
		return rollback(err)
	}
	existing := make(map[string]persistedNode)
	for existingRows.Next() {
		var persisted persistedNode
		if err := existingRows.Scan(
			&persisted.node.RawURI,
			&persisted.node.Type,
			&persisted.node.Name,
			&persisted.node.Disabled,
			&persisted.node.SourceID,
			&persisted.sortOrder,
		); err != nil {
			_ = existingRows.Close()
			return rollback(err)
		}
		existing[persisted.node.RawURI] = persisted
	}
	if err := existingRows.Err(); err != nil {
		_ = existingRows.Close()
		return rollback(err)
	}
	_ = existingRows.Close()

	upsertStmt, err := tx.Prepare(`INSERT INTO nodes
		(raw_uri, type, name, disabled, source_id, sort_order)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(raw_uri) DO UPDATE SET
			type = excluded.type,
			name = excluded.name,
			disabled = excluded.disabled,
			source_id = excluded.source_id,
			sort_order = excluded.sort_order`)
	if err != nil {
		return rollback(err)
	}
	desired := make(map[string]bool, len(nodeList))
	for index, n := range nodeList {
		desired[n.RawURI] = true
		persisted, exists := existing[n.RawURI]
		if exists &&
			persisted.node.Type == n.Type &&
			persisted.node.Name == n.Name &&
			persisted.node.Disabled == n.Disabled &&
			persisted.node.SourceID == n.SourceID &&
			persisted.sortOrder == index {
			continue
		}
		if _, err := upsertStmt.Exec(
			n.RawURI, n.Type, n.Name, n.Disabled, n.SourceID, index,
		); err != nil {
			_ = upsertStmt.Close()
			return rollback(err)
		}
	}
	_ = upsertStmt.Close()

	deleteStmt, err := tx.Prepare("DELETE FROM nodes WHERE raw_uri = ?")
	if err != nil {
		return rollback(err)
	}
	nodesDeleted := false
	for rawURI := range existing {
		if desired[rawURI] {
			continue
		}
		if _, err := deleteStmt.Exec(rawURI); err != nil {
			_ = deleteStmt.Close()
			return rollback(err)
		}
		nodesDeleted = true
	}
	_ = deleteStmt.Close()

	type sourceKey struct {
		sourceID int64
		rawURI   string
	}
	sourceRows, err := tx.Query(
		"SELECT subscription_id, raw_uri FROM proxy_subscription_nodes",
	)
	if err != nil {
		return rollback(err)
	}
	existingSources := make(map[sourceKey]bool)
	for sourceRows.Next() {
		var key sourceKey
		if err := sourceRows.Scan(&key.sourceID, &key.rawURI); err != nil {
			_ = sourceRows.Close()
			return rollback(err)
		}
		existingSources[key] = true
	}
	if err := sourceRows.Err(); err != nil {
		_ = sourceRows.Close()
		return rollback(err)
	}
	_ = sourceRows.Close()

	desiredSources := make(map[sourceKey]bool)
	for rawURI, sourceIDs := range subscriptionSources {
		if !desired[rawURI] {
			continue
		}
		for sourceID := range sourceIDs {
			desiredSources[sourceKey{sourceID: sourceID, rawURI: rawURI}] = true
		}
	}

	sourceInsertStmt, err := tx.Prepare(`INSERT INTO proxy_subscription_nodes(subscription_id, raw_uri)
		VALUES (?, ?)`)
	if err != nil {
		return rollback(err)
	}
	sourcesChanged := false
	for key := range desiredSources {
		if existingSources[key] {
			continue
		}
		if _, err := sourceInsertStmt.Exec(key.sourceID, key.rawURI); err != nil {
			_ = sourceInsertStmt.Close()
			return rollback(err)
		}
		sourcesChanged = true
	}
	_ = sourceInsertStmt.Close()

	sourceDeleteStmt, err := tx.Prepare(
		"DELETE FROM proxy_subscription_nodes WHERE subscription_id = ? AND raw_uri = ?",
	)
	if err != nil {
		return rollback(err)
	}
	for key := range existingSources {
		if desiredSources[key] {
			continue
		}
		if _, err := sourceDeleteStmt.Exec(key.sourceID, key.rawURI); err != nil {
			_ = sourceDeleteStmt.Close()
			return rollback(err)
		}
		sourcesChanged = true
	}
	_ = sourceDeleteStmt.Close()
	if sourcesChanged || nodesDeleted {
		if _, err := tx.Exec(`UPDATE proxy_subscriptions
			SET node_count = (
				SELECT COUNT(*) FROM proxy_subscription_nodes psn
				WHERE psn.subscription_id = proxy_subscriptions.id
			)`); err != nil {
			return rollback(err)
		}
	}
	if finalize != nil {
		if err := finalize(tx); err != nil {
			return rollback(err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交节点事务: %w", err)
	}
	return nil
}

type healthUpdate struct {
	database *sql.DB
	uri      string
	h        NodeHealth
}

type healthUpdateKey struct {
	database *sql.DB
	uri      string
}

var (
	healthUpdateChan chan healthUpdate                        //nolint:gochecknoglobals
	healthFlushChan  chan chan error                          //nolint:gochecknoglobals
	healthOnce       sync.Once                                //nolint:gochecknoglobals
	healthOverflowMu sync.Mutex                               //nolint:gochecknoglobals
	healthOverflow   = make(map[healthUpdateKey]healthUpdate) //nolint:gochecknoglobals
)

func enqueueHealthUpdate(update healthUpdate) {
	select {
	case healthUpdateChan <- update:
	default:
		// Never block node selection or admin operations on a slow/locked DB.
		// Coalesce overflow by database and URI so the newest state is retried
		// without carrying writes across database lifecycle boundaries.
		healthOverflowMu.Lock()
		healthOverflow[healthUpdateKey{database: update.database, uri: update.uri}] = update
		healthOverflowMu.Unlock()
	}
}

func mergeHealthOverflow(batch map[healthUpdateKey]healthUpdate) {
	healthOverflowMu.Lock()
	for key, update := range healthOverflow {
		batch[key] = update
		delete(healthOverflow, key)
	}
	healthOverflowMu.Unlock()
}

func initHealthQueue() {
	healthUpdateChan = make(chan healthUpdate, 2048)
	healthFlushChan = make(chan chan error)
	go func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()

		batch := make(map[healthUpdateKey]healthUpdate)

		flush := func() error {
			mergeHealthOverflow(batch)
			database := db.CurrentDB()
			for key := range batch {
				if key.database != database {
					delete(batch, key)
				}
			}
			if len(batch) == 0 || database == nil {
				return nil
			}
			tx, err := database.Begin()
			if err != nil {
				log.Printf("[ERROR] Failed to begin health save transaction: %v", err)
				if len(batch) > 1000 {
					for k := range batch {
						delete(batch, k)
					}
				}
				return err
			}
			stmt, err := tx.Prepare(`INSERT INTO node_health
				(raw_uri, success_count, fail_count, consecutive_failures, last_test_ms,
				last_test_error, last_success_at, last_fail_at, cooldown_until,
				last_429_at, rate_limit_count, recent_use_count, last_selected_at)
				SELECT ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?
				WHERE EXISTS (SELECT 1 FROM nodes WHERE raw_uri = ?)
				ON CONFLICT(raw_uri) DO UPDATE SET
					success_count = excluded.success_count,
					fail_count = excluded.fail_count,
					consecutive_failures = excluded.consecutive_failures,
					last_test_ms = excluded.last_test_ms,
					last_test_error = excluded.last_test_error,
					last_success_at = excluded.last_success_at,
					last_fail_at = excluded.last_fail_at,
					cooldown_until = excluded.cooldown_until,
					last_429_at = excluded.last_429_at,
					rate_limit_count = excluded.rate_limit_count,
					recent_use_count = excluded.recent_use_count,
					last_selected_at = excluded.last_selected_at`)
			if err != nil {
				_ = tx.Rollback()
				log.Printf("[ERROR] Failed to prepare health save statement: %v", err)
				if len(batch) > 1000 {
					for k := range batch {
						delete(batch, k)
					}
				}
				return err
			}
			defer stmt.Close()

			for _, update := range batch {
				uri := update.uri
				h := update.h
				if _, err := stmt.Exec(
					uri, h.SuccessCount, h.FailCount, h.ConsecutiveFailures,
					h.LastTestMs, h.LastTestError, h.LastSuccessAt, h.LastFailAt,
					h.CooldownUntil, h.Last429At, h.RateLimitCount,
					h.RecentUseCount, h.LastSelectedAt, uri,
				); err != nil {
					_ = tx.Rollback()
					log.Printf("[ERROR] Failed to persist node health: %v", err)
					return err
				}
			}
			if err := tx.Commit(); err != nil {
				log.Printf("[ERROR] Failed to commit node health: %v", err)
				return err
			}
			for k := range batch {
				delete(batch, k)
			}
			return nil
		}

		for {
			select {
			case update, ok := <-healthUpdateChan:
				if !ok {
					_ = flush()
					return
				}
				key := healthUpdateKey{database: update.database, uri: update.uri}
				batch[key] = update
				if len(batch) >= 100 {
					_ = flush()
				}
			case response := <-healthFlushChan:
				// A flush is a persistence barrier. Drain updates that were queued
				// before the request so shutdown cannot acknowledge while older
				// health mutations are still waiting in the buffered channel.
			drain:
				for {
					select {
					case update := <-healthUpdateChan:
						key := healthUpdateKey{database: update.database, uri: update.uri}
						batch[key] = update
					default:
						break drain
					}
				}
				response <- flush()
			case <-ticker.C:
				_ = flush()
			}
		}
	}()
}

// FlushHealth waits until all health mutations queued before this call have
// been persisted. It is used as a shutdown barrier before SQLite is closed.
func FlushHealth(ctx context.Context) error {
	healthOnce.Do(initHealthQueue)
	response := make(chan error, 1)
	select {
	case healthFlushChan <- response:
	case <-ctx.Done():
		return ctx.Err() //nolint:wrapcheck
	}
	select {
	case err := <-response:
		return err
	case <-ctx.Done():
		return ctx.Err() //nolint:wrapcheck
	}
}

func saveHealthUnsafe() {
	database := db.CurrentDB()
	if database == nil {
		return
	}
	healthOnce.Do(initHealthQueue)
	for uri, h := range healthMap {
		if h != nil {
			enqueueHealthUpdate(healthUpdate{database: database, uri: uri, h: *h})
		}
	}
}

func updateSingleNodeHealthUnsafe(uri string, h *NodeHealth) {
	database := db.CurrentDB()
	if database == nil || h == nil {
		return
	}
	healthOnce.Do(initHealthQueue)
	enqueueHealthUpdate(healthUpdate{database: database, uri: uri, h: *h})
}

func updateSingleNodeDisabledUnsafe(uri string, disabled bool) {
	database := db.CurrentDB()
	if database == nil {
		return
	}
	_, _ = database.Exec("UPDATE nodes SET disabled = ? WHERE raw_uri = ?", disabled, uri)
}

type TestProgress struct {
	Running     bool   `json:"running"`
	Paused      bool   `json:"paused"`
	Terminated  bool   `json:"terminated"`
	Total       int    `json:"total"`
	Done        int    `json:"done"`
	OkCount     int    `json:"ok_count"`
	FailCount   int    `json:"fail_count"`
	Incomplete  int    `json:"incomplete"`
	CurrentNode string `json:"current_node"`
}

var (
	//nolint:gochecknoglobals // Test progress lock
	progressMu sync.RWMutex
	//nolint:gochecknoglobals // Test progress state
	globalProgress TestProgress
	//nolint:gochecknoglobals // Test progress control cond
	testControlCond = sync.NewCond(&progressMu)
)

func GetTestProgress() TestProgress {
	progressMu.RLock()
	defer progressMu.RUnlock()
	return globalProgress
}

func StartTestProgress(total int) {
	_ = TryStartTestProgress(total)
}

func TryStartTestProgress(total int) bool {
	progressMu.Lock()
	defer progressMu.Unlock()
	if globalProgress.Running {
		return false
	}
	globalProgress = TestProgress{
		Running:     true,
		Paused:      false,
		Terminated:  false,
		Total:       total,
		Done:        0,
		OkCount:     0,
		FailCount:   0,
		Incomplete:  0,
		CurrentNode: "准备中...",
	}
	return true
}

func UpdateTestProgress(nodeName string, ok bool) {
	progressMu.Lock()
	defer progressMu.Unlock()
	if !globalProgress.Running || globalProgress.Terminated {
		return
	}
	globalProgress.Done++
	if ok {
		globalProgress.OkCount++
	} else {
		globalProgress.FailCount++
	}
	globalProgress.CurrentNode = nodeName
}

func FinishTestProgress() {
	progressMu.Lock()
	defer progressMu.Unlock()
	globalProgress.Running = false
	globalProgress.Paused = false
	globalProgress.Incomplete = max(0, globalProgress.Total-globalProgress.Done)
	if globalProgress.Terminated {
		globalProgress.CurrentNode = "已终止"
	} else {
		globalProgress.CurrentNode = "测试完成"
	}
	testControlCond.Broadcast()
}

func PauseTestProgress() {
	progressMu.Lock()
	defer progressMu.Unlock()
	if globalProgress.Running && !globalProgress.Terminated {
		globalProgress.Paused = true
		globalProgress.CurrentNode = "已暂停..."
	}
}

func ResumeTestProgress() {
	progressMu.Lock()
	defer progressMu.Unlock()
	if globalProgress.Running && globalProgress.Paused {
		globalProgress.Paused = false
		globalProgress.CurrentNode = "恢复测试中..."
		testControlCond.Broadcast()
	}
}

func TerminateTestProgress() {
	progressMu.Lock()
	defer progressMu.Unlock()
	if globalProgress.Running {
		globalProgress.Terminated = true
		globalProgress.Paused = false
		globalProgress.CurrentNode = "正在终止..."
		testControlCond.Broadcast()
	}
}

func CheckTestControl() bool {
	progressMu.Lock()
	defer progressMu.Unlock()
	for globalProgress.Running && globalProgress.Paused && !globalProgress.Terminated {
		testControlCond.Wait()
	}
	return !globalProgress.Running || globalProgress.Terminated
}

func pruneHealthUnsafe() {
	for uri := range healthMap {
		found := false
		for _, n := range nodeList {
			if n.RawURI == uri {
				found = true
				break
			}
		}
		if !found {
			delete(healthMap, uri)
		}
	}
}

func cloneSubscriptionSourcesUnsafe() map[string]map[int64]bool {
	out := make(map[string]map[int64]bool, len(subscriptionSources))
	for rawURI, sourceIDs := range subscriptionSources {
		copied := make(map[int64]bool, len(sourceIDs))
		for sourceID, present := range sourceIDs {
			copied[sourceID] = present
		}
		out[rawURI] = copied
	}
	return out
}

func cloneHealthMapUnsafe() map[string]*NodeHealth {
	out := make(map[string]*NodeHealth, len(healthMap))
	for rawURI, health := range healthMap {
		if health == nil {
			out[rawURI] = nil
			continue
		}
		copied := *health
		out[rawURI] = &copied
	}
	return out
}

// ReplaceManualNodes atomically replaces manually managed nodes while
// preserving subscription ownership, shared nodes and health state for
// unchanged URIs.
func ReplaceManualNodes(newNodes []Node) error {
	mu.Lock()
	ensureLoaded()
	previousNodes := append([]Node(nil), nodeList...)
	previousSources := cloneSubscriptionSourcesUnsafe()
	previousHealth := cloneHealthMapUnsafe()

	desired := make(map[string]Node, len(newNodes))
	desiredOrder := make([]string, 0, len(newNodes))
	for _, node := range newNodes {
		node.RawURI = strings.TrimSpace(node.RawURI)
		if node.RawURI == "" {
			continue
		}
		node.SourceID = 0
		if _, exists := desired[node.RawURI]; !exists {
			desiredOrder = append(desiredOrder, node.RawURI)
		}
		desired[node.RawURI] = node
	}

	existing := make(map[string]Node, len(nodeList))
	nextNodes := make([]Node, 0, len(nodeList)+len(desired))
	keptURIs := make(map[string]bool, len(nodeList)+len(desired))
	for _, current := range nodeList {
		existing[current.RawURI] = current
		sources := subscriptionSources[current.RawURI]
		if len(sources) == 0 {
			continue
		}
		if replacement, replace := desired[current.RawURI]; replace {
			replacement.Disabled = current.Disabled
			replacement.SourceID = 0
			nextNodes = append(nextNodes, replacement)
			keptURIs[current.RawURI] = true
			delete(desired, current.RawURI)
			continue
		}
		var smallestSourceID int64
		for sourceID := range sources {
			if smallestSourceID == 0 || sourceID < smallestSourceID {
				smallestSourceID = sourceID
			}
		}
		current.SourceID = smallestSourceID
		nextNodes = append(nextNodes, current)
		keptURIs[current.RawURI] = true
	}
	for _, rawURI := range desiredOrder {
		replacement, exists := desired[rawURI]
		if !exists {
			continue
		}
		if current, found := existing[rawURI]; found {
			replacement.Disabled = current.Disabled
		}
		nextNodes = append(nextNodes, replacement)
		keptURIs[rawURI] = true
	}

	removedURIs := make([]string, 0)
	for _, current := range nodeList {
		if !keptURIs[current.RawURI] {
			removedURIs = append(removedURIs, current.RawURI)
			delete(subscriptionSources, current.RawURI)
			delete(healthMap, current.RawURI)
		}
	}
	nodeList = nextNodes
	if err := saveNodesUnsafe(); err != nil {
		nodeList = previousNodes
		subscriptionSources = previousSources
		healthMap = previousHealth
		mu.Unlock()
		return err
	}
	for _, rawURI := range removedURIs {
		globalStickyPool.Evict(rawURI)
	}
	cb := DeleteNodeCallback
	mu.Unlock()
	if cb != nil {
		for _, rawURI := range removedURIs {
			cb(rawURI)
		}
	}
	return nil
}

func MergeNodes(newNodes []Node) error {
	mu.Lock()
	defer mu.Unlock()
	ensureLoaded()
	previousNodes := append([]Node(nil), nodeList...)
	previousHealth := cloneHealthMapUnsafe()
	existing := make(map[string]int)
	for i, n := range nodeList {
		existing[n.RawURI] = i
	}
	for _, n := range newNodes {
		n.RawURI = strings.TrimSpace(n.RawURI)
		if n.RawURI == "" {
			continue
		}
		n.SourceID = 0
		if index, found := existing[n.RawURI]; found {
			// 手动导入与订阅节点重合时，标记为手动节点；订阅关系仍会保留。
			nodeList[index].SourceID = 0
			continue
		}
		nodeList = append(nodeList, n)
		existing[n.RawURI] = len(nodeList) - 1
	}
	pruneHealthUnsafe()
	if err := saveNodesUnsafe(); err != nil {
		nodeList = previousNodes
		healthMap = previousHealth
		log.Printf("[Nodes] 合并节点持久化失败，已回滚内存状态: %v", err)
		return err
	}
	return nil
}

type SubscriptionNodeSyncResult struct {
	Count   int
	Added   int
	Removed int
}

// SyncSubscriptionNodes 差异同步某个订阅产生的节点。
// URI 未变化的节点会保留禁用状态、健康记录、冷却状态及现有连接。
func SyncSubscriptionNodes(
	sourceID int64,
	newNodes []Node,
) (SubscriptionNodeSyncResult, error) {
	return syncSubscriptionNodes(sourceID, newNodes, nil)
}

// SyncSubscriptionNodesAndMarkRefreshed 原子提交节点关系和订阅刷新状态。
func SyncSubscriptionNodesAndMarkRefreshed(
	sourceID int64,
	newNodes []Node,
) (SubscriptionNodeSyncResult, error) {
	return syncSubscriptionNodes(
		sourceID,
		newNodes,
		func(tx *sql.Tx, result SubscriptionNodeSyncResult) error {
			now := time.Now().Unix()
			updateResult, err := tx.Exec(`UPDATE proxy_subscriptions SET
				last_refreshed_at = ?, last_attempt_at = ?, last_error = '',
				consecutive_failures = 0, node_count = ?, updated_at = ?
				WHERE id = ?`, now, now, result.Count, now, sourceID)
			if err != nil {
				return fmt.Errorf("update proxy subscription refresh state: %w", err)
			}
			if affected, _ := updateResult.RowsAffected(); affected != 1 {
				return errors.New("proxy subscription not found")
			}
			return nil
		},
	)
}

func syncSubscriptionNodes(
	sourceID int64,
	newNodes []Node,
	finalize func(*sql.Tx, SubscriptionNodeSyncResult) error,
) (SubscriptionNodeSyncResult, error) {
	if sourceID <= 0 {
		return SubscriptionNodeSyncResult{}, errors.New("subscription source ID must be positive")
	}

	mu.Lock()
	ensureLoaded()
	database := db.CurrentDB()
	if database == nil {
		mu.Unlock()
		return SubscriptionNodeSyncResult{}, errors.New("database unavailable")
	}
	var sourceExists bool
	if err := database.QueryRow(
		"SELECT EXISTS(SELECT 1 FROM proxy_subscriptions WHERE id = ?)",
		sourceID,
	).Scan(&sourceExists); err != nil {
		mu.Unlock()
		return SubscriptionNodeSyncResult{}, fmt.Errorf("检查代理订阅: %w", err)
	}
	if !sourceExists {
		mu.Unlock()
		return SubscriptionNodeSyncResult{}, errors.New("proxy subscription not found")
	}
	previousNodes := append([]Node(nil), nodeList...)
	previousSources := cloneSubscriptionSourcesUnsafe()
	previousHealth := cloneHealthMapUnsafe()
	result := SubscriptionNodeSyncResult{}
	desired := make(map[string]Node, len(newNodes))
	desiredOrder := make([]string, 0, len(newNodes))
	for _, n := range newNodes {
		n.RawURI = strings.TrimSpace(n.RawURI)
		if n.RawURI == "" {
			continue
		}
		if _, exists := desired[n.RawURI]; !exists {
			desiredOrder = append(desiredOrder, n.RawURI)
		}
		desired[n.RawURI] = n
	}

	previousMembership := make(map[string]bool)
	for rawURI, sourceIDs := range subscriptionSources {
		if sourceIDs[sourceID] {
			previousMembership[rawURI] = true
		}
	}

	nodeIndexes := make(map[string]int, len(nodeList))
	for i := range nodeList {
		nodeIndexes[nodeList[i].RawURI] = i
	}
	for _, uri := range desiredOrder {
		next := desired[uri]
		if subscriptionSources[uri] == nil {
			subscriptionSources[uri] = make(map[int64]bool)
		}
		subscriptionSources[uri][sourceID] = true
		if index, exists := nodeIndexes[uri]; exists {
			current := nodeList[index]
			if current.SourceID != 0 && (current.SourceID == sourceID || sourceID < current.SourceID) {
				next.SourceID = sourceID
				next.Disabled = current.Disabled
				if strings.TrimSpace(next.Name) == "" {
					next.Name = current.Name
				}
				nodeList[index] = next
			}
		} else {
			next.SourceID = sourceID
			nodeList = append(nodeList, next)
			nodeIndexes[uri] = len(nodeList) - 1
			result.Added++
		}
		delete(previousMembership, uri)
		result.Count++
	}

	for uri := range previousMembership {
		delete(subscriptionSources[uri], sourceID)
		if len(subscriptionSources[uri]) == 0 {
			delete(subscriptionSources, uri)
		}
	}

	kept := make([]Node, 0, len(nodeList))
	var removedURIs []string
	for _, n := range nodeList {
		sources := subscriptionSources[n.RawURI]
		if n.SourceID == 0 {
			kept = append(kept, n)
			continue
		}
		if len(sources) > 0 {
			var smallest int64
			for currentSourceID := range sources {
				if smallest == 0 || currentSourceID < smallest {
					smallest = currentSourceID
				}
			}
			n.SourceID = smallest
			kept = append(kept, n)
			continue
		}

		result.Removed++
		removedURIs = append(removedURIs, n.RawURI)
		delete(subscriptionSources, n.RawURI)
		delete(healthMap, n.RawURI)
	}
	nodeList = kept
	var persistFinalize func(*sql.Tx) error
	if finalize != nil {
		persistFinalize = func(tx *sql.Tx) error {
			return finalize(tx, result)
		}
	}
	if err := saveNodesUnsafeWithTx(persistFinalize); err != nil {
		nodeList = previousNodes
		subscriptionSources = previousSources
		healthMap = previousHealth
		mu.Unlock()
		return SubscriptionNodeSyncResult{}, err
	}
	for _, rawURI := range removedURIs {
		globalStickyPool.Evict(rawURI)
	}
	if result.Removed > 0 {
		saveHealthUnsafe()
	}
	cb := DeleteNodeCallback
	mu.Unlock()

	if cb != nil {
		for _, uri := range removedURIs {
			cb(uri)
		}
	}
	return result, nil
}

// DeleteProxySubscriptionAndNodes 原子删除订阅归属、仅由该订阅拥有的节点和订阅元数据。
func DeleteProxySubscriptionAndNodes(sourceID int64) (int, error) {
	result, err := syncSubscriptionNodes(
		sourceID,
		nil,
		func(tx *sql.Tx, _ SubscriptionNodeSyncResult) error {
			deleteResult, err := tx.Exec("DELETE FROM proxy_subscriptions WHERE id = ?", sourceID)
			if err != nil {
				return fmt.Errorf("delete proxy subscription: %w", err)
			}
			if affected, _ := deleteResult.RowsAffected(); affected != 1 {
				return errors.New("proxy subscription not found")
			}
			return nil
		},
	)
	return result.Removed, err
}

// ReplaceSubscriptionNodes 保留旧调用约定，返回本次新增和删除数量。
func ReplaceSubscriptionNodes(sourceID int64, newNodes []Node) (added, removed int) {
	result, err := SyncSubscriptionNodes(sourceID, newNodes)
	if err != nil {
		log.Printf("[Nodes] 替换订阅节点失败: %v", err)
		return 0, 0
	}
	return result.Added, result.Removed
}

func DeleteSubscriptionNodes(sourceID int64) (int, error) {
	result, err := SyncSubscriptionNodes(sourceID, nil)
	return result.Removed, err
}

func DeleteNode(uri string) {
	mu.Lock()
	ensureLoaded()
	previousNodes := append([]Node(nil), nodeList...)
	previousSources := cloneSubscriptionSourcesUnsafe()
	previousHealth := cloneHealthMapUnsafe()
	var kept []Node
	for _, n := range nodeList {
		if n.RawURI != uri {
			kept = append(kept, n)
		}
	}
	nodeList = kept
	delete(subscriptionSources, uri)
	delete(healthMap, uri)
	if err := saveNodesUnsafe(); err != nil {
		nodeList = previousNodes
		subscriptionSources = previousSources
		healthMap = previousHealth
		mu.Unlock()
		log.Printf("[Nodes] 删除节点持久化失败，已回滚内存状态: %v", err)
		return
	}
	globalStickyPool.Evict(uri)
	saveHealthUnsafe()
	cb := DeleteNodeCallback
	mu.Unlock() // 必须先解锁，避免底层的销毁回调查找节点名称时发生死锁
	if cb != nil {
		cb(uri)
	}
}

func DedupNodes() int {
	mu.Lock()
	ensureLoaded()
	previousNodes := append([]Node(nil), nodeList...)
	previousSources := cloneSubscriptionSourcesUnsafe()
	previousHealth := cloneHealthMapUnsafe()
	keepMap := make(map[string]int)
	var kept []Node
	removed := 0
	var removedURIs []string
	for _, n := range nodeList {
		key := n.RawURI
		if scheme, userinfo, host, port, ok := parseNodeIdentity(n.RawURI); ok {
			key = scheme + "://" + userinfo + "@" + host + ":" + strconv.Itoa(port)
		}
		if _, exists := keepMap[key]; !exists {
			keepMap[key] = len(kept)
			kept = append(kept, n)
		} else {
			keptIndex := keepMap[key]
			keptURI := kept[keptIndex].RawURI
			if subscriptionSources[keptURI] == nil {
				subscriptionSources[keptURI] = make(map[int64]bool)
			}
			for sourceID := range subscriptionSources[n.RawURI] {
				subscriptionSources[keptURI][sourceID] = true
			}
			if n.SourceID == 0 {
				kept[keptIndex].SourceID = 0
			} else if kept[keptIndex].SourceID != 0 {
				for sourceID := range subscriptionSources[keptURI] {
					if sourceID < kept[keptIndex].SourceID {
						kept[keptIndex].SourceID = sourceID
					}
				}
			}
			removed++
			removedURIs = append(removedURIs, n.RawURI)
			delete(subscriptionSources, n.RawURI)
			delete(healthMap, n.RawURI)
		}
	}
	nodeList = kept
	if err := saveNodesUnsafe(); err != nil {
		nodeList = previousNodes
		subscriptionSources = previousSources
		healthMap = previousHealth
		mu.Unlock()
		log.Printf("[Nodes] 节点去重持久化失败，已回滚内存状态: %v", err)
		return 0
	}
	for _, rawURI := range removedURIs {
		globalStickyPool.Evict(rawURI)
	}
	saveHealthUnsafe()
	cb := DeleteNodeCallback
	mu.Unlock() // 先解锁再通知销毁连接池

	if cb != nil {
		for _, u := range removedURIs {
			cb(u)
		}
	}
	return removed
}

func DeleteDisabled() int {
	mu.Lock()
	ensureLoaded()
	previousNodes := append([]Node(nil), nodeList...)
	previousSources := cloneSubscriptionSourcesUnsafe()
	previousHealth := cloneHealthMapUnsafe()
	var kept []Node
	removed := 0
	var removedURIs []string
	for _, n := range nodeList {
		if !n.Disabled {
			kept = append(kept, n)
		} else {
			removed++
			removedURIs = append(removedURIs, n.RawURI)
			delete(subscriptionSources, n.RawURI)
			delete(healthMap, n.RawURI)
		}
	}
	nodeList = kept
	if err := saveNodesUnsafe(); err != nil {
		nodeList = previousNodes
		subscriptionSources = previousSources
		healthMap = previousHealth
		mu.Unlock()
		log.Printf("[Nodes] 删除禁用节点持久化失败，已回滚内存状态: %v", err)
		return 0
	}
	for _, rawURI := range removedURIs {
		globalStickyPool.Evict(rawURI)
	}
	saveHealthUnsafe()
	cb := DeleteNodeCallback
	mu.Unlock()

	if cb != nil {
		for _, u := range removedURIs {
			cb(u)
		}
	}
	return removed
}

func BatchUpdateNodesDisabled(uris []string, disabled bool) error {
	mu.Lock()
	defer mu.Unlock()
	ensureLoaded()
	previousNodes := append([]Node(nil), nodeList...)
	targets := make(map[string]bool)
	for _, u := range uris {
		targets[u] = true
	}
	for i, n := range nodeList {
		if targets[n.RawURI] {
			nodeList[i].Disabled = disabled
		}
	}
	database := db.CurrentDB()
	if database == nil || len(uris) == 0 {
		return nil
	}
	tx, err := database.Begin()
	if err != nil {
		nodeList = previousNodes
		return fmt.Errorf("开始批量更新节点事务: %w", err)
	}
	rollback := func(updateErr error) error {
		_ = tx.Rollback()
		nodeList = previousNodes
		return fmt.Errorf("批量更新节点: %w", updateErr)
	}
	stmt, err := tx.Prepare("UPDATE nodes SET disabled = ? WHERE raw_uri = ?")
	if err != nil {
		return rollback(err)
	}
	for _, u := range uris {
		if _, err := stmt.Exec(disabled, u); err != nil {
			_ = stmt.Close()
			return rollback(err)
		}
	}
	if err := stmt.Close(); err != nil {
		return rollback(err)
	}
	if err := tx.Commit(); err != nil {
		nodeList = previousNodes
		return fmt.Errorf("提交批量更新节点事务: %w", err)
	}
	return nil
}

func BatchDeleteNodes(uris []string) error {
	mu.Lock()
	ensureLoaded()
	previousNodes := append([]Node(nil), nodeList...)
	previousSources := cloneSubscriptionSourcesUnsafe()
	previousHealth := cloneHealthMapUnsafe()
	targets := make(map[string]bool)
	for _, u := range uris {
		targets[u] = true
		delete(subscriptionSources, u)
		delete(healthMap, u)
	}
	var kept []Node
	for _, n := range nodeList {
		if !targets[n.RawURI] {
			kept = append(kept, n)
		}
	}
	nodeList = kept
	if err := saveNodesUnsafe(); err != nil {
		nodeList = previousNodes
		subscriptionSources = previousSources
		healthMap = previousHealth
		mu.Unlock()
		log.Printf("[Nodes] 批量删除节点持久化失败，已回滚内存状态: %v", err)
		return err
	}
	for _, rawURI := range uris {
		globalStickyPool.Evict(rawURI)
	}
	saveHealthUnsafe()
	cb := DeleteNodeCallback
	mu.Unlock() // 防止在批量删除时引发卡死死锁

	if cb != nil {
		for _, u := range uris {
			cb(u)
		}
	}
	return nil
}

func SortNodesByLatency() {
	mu.Lock()
	ensureLoaded()
	previousNodes := append([]Node(nil), nodeList...)

	sort.Slice(nodeList, func(i, j int) bool {
		n1 := nodeList[i]
		n2 := nodeList[j]

		// 禁用的排在最后面
		if n1.Disabled != n2.Disabled {
			return !n1.Disabled
		}

		h1 := healthMap[n1.RawURI]
		h2 := healthMap[n2.RawURI]

		val1 := math.MaxFloat64
		if h1 != nil {
			if h1.ConsecutiveFailures > 0 {
				val1 = 1e6 + float64(h1.ConsecutiveFailures)*1000
			} else if h1.LastTestMs > 0 {
				val1 = h1.LastTestMs
			}
		}

		val2 := math.MaxFloat64
		if h2 != nil {
			if h2.ConsecutiveFailures > 0 {
				val2 = 1e6 + float64(h2.ConsecutiveFailures)*1000
			} else if h2.LastTestMs > 0 {
				val2 = h2.LastTestMs
			}
		}

		// 延迟一致的按名字自然排序
		if val1 == val2 {
			return n1.Name < n2.Name
		}
		return val1 < val2
	})

	if err := saveNodesUnsafe(); err != nil {
		nodeList = previousNodes
		log.Printf("[Nodes] 保存节点排序失败，已回滚内存状态: %v", err)
	}
	mu.Unlock()
}

func SortNodesByLatencyDesc() {
	mu.Lock()
	ensureLoaded()
	previousNodes := append([]Node(nil), nodeList...)

	sort.Slice(nodeList, func(i, j int) bool {
		n1 := nodeList[i]
		n2 := nodeList[j]

		// 禁用的排在最后面
		if n1.Disabled != n2.Disabled {
			return !n1.Disabled
		}

		h1 := healthMap[n1.RawURI]
		h2 := healthMap[n2.RawURI]

		val1 := math.MaxFloat64
		if h1 != nil {
			if h1.ConsecutiveFailures > 0 {
				val1 = 1e6 + float64(h1.ConsecutiveFailures)*1000
			} else if h1.LastTestMs > 0 {
				val1 = h1.LastTestMs
			}
		}

		val2 := math.MaxFloat64
		if h2 != nil {
			if h2.ConsecutiveFailures > 0 {
				val2 = 1e6 + float64(h2.ConsecutiveFailures)*1000
			} else if h2.LastTestMs > 0 {
				val2 = h2.LastTestMs
			}
		}

		// 延迟一致的按名字自然排序
		if val1 == val2 {
			return n1.Name < n2.Name
		}
		// 这里改为降序，val1 > val2
		return val1 > val2
	})

	if err := saveNodesUnsafe(); err != nil {
		nodeList = previousNodes
		log.Printf("[Nodes] 保存节点排序失败，已回滚内存状态: %v", err)
	}
	mu.Unlock()
}

func GetNodeName(uri string) string {
	lockLoadedForRead()
	node, found := lookupNodeUnsafe(uri)
	mu.RUnlock()
	if !found {
		return "Unknown"
	}
	return SafeNodeLabel(node.Name)
}

// SafeNodeLabel returns a single-line, bounded label suitable for logs.
// Node names may originate from untrusted subscription providers.
func SafeNodeLabel(name string) string {
	const maxRunes = 128
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return "Unnamed"
	}
	if nodeLabelAlreadySafe(trimmed, maxRunes) {
		return trimmed
	}

	var builder strings.Builder
	count := 0
	spacePending := false
	for _, r := range trimmed {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			continue
		}
		if unicode.IsSpace(r) {
			spacePending = builder.Len() > 0
			continue
		}
		if spacePending && count < maxRunes {
			builder.WriteByte(' ')
			count++
		}
		spacePending = false
		if count >= maxRunes {
			break
		}
		builder.WriteRune(r)
		count++
	}
	if builder.Len() == 0 {
		return "Unnamed"
	}
	return builder.String()
}

func nodeLabelAlreadySafe(name string, maxRunes int) bool {
	count := 0
	previousSpace := false
	for _, r := range name {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return false
		}
		if unicode.IsSpace(r) {
			// The slow path normalizes all Unicode whitespace and repeated ASCII
			// spaces to a single ordinary space.
			if r != ' ' || previousSpace {
				return false
			}
			previousSpace = true
		} else {
			previousSpace = false
		}
		count++
		if count > maxRunes {
			return false
		}
	}
	return true
}

func EnableNode(uri string) bool {
	mu.Lock()
	defer mu.Unlock()
	ensureLoaded()
	found := false
	for i, n := range nodeList {
		if n.RawURI == uri {
			nodeList[i].Disabled = false
			if h, exists := healthMap[uri]; exists {
				h.CooldownUntil = 0
				updateSingleNodeHealthUnsafe(uri, h)
			}
			updateSingleNodeDisabledUnsafe(uri, false)
			found = true
			break
		}
	}
	return found
}

func padB64(s string) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "-", "+"), "_", "/")
	if pad := len(s) % 4; pad != 0 {
		s += strings.Repeat("=", 4-pad)
	}
	return s
}

func parseNodeIdentity(rawURI string) (scheme, userinfo, host string, port int, ok bool) {
	if strings.HasPrefix(rawURI, "vmess://") {
		b64Str := rawURI[8:]
		if idx := strings.Index(b64Str, "?"); idx != -1 {
			b64Str = b64Str[:idx]
		}
		if idx := strings.Index(b64Str, "#"); idx != -1 {
			b64Str = b64Str[:idx]
		}
		b64Str = padB64(b64Str)
		if b, err := base64.StdEncoding.DecodeString(b64Str); err == nil {
			var d map[string]any
			if err := json.Unmarshal(b, &d); err == nil {
				id, _ := d["id"].(string)
				add, _ := d["add"].(string)
				portStr := fmt.Sprintf("%v", d["port"])
				p, _ := strconv.Atoi(portStr)
				return "vmess", id, add, p, true
			}
		}
		return "", "", "", 0, false
	}
	if strings.HasPrefix(rawURI, "ss://") {
		body := rawURI[5:]
		if idx := strings.Index(body, "#"); idx != -1 {
			body = body[:idx]
		}
		if idx := strings.Index(body, "@"); idx != -1 {
			b, err := base64.StdEncoding.DecodeString(padB64(body[:idx]))
			if err == nil {
				parts := strings.SplitN(string(b), ":", 2)
				if len(parts) >= 2 {
					hp := strings.Split(body[idx+1:], ":")
					if len(hp) >= 2 {
						p, _ := strconv.Atoi(hp[1])
						return "ss", parts[0] + ":" + parts[1], hp[0], p, true
					}
				}
			}
		}
		return "", "", "", 0, false
	}
	u, err := url.Parse(rawURI)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", "", "", 0, false
	}
	scheme = u.Scheme
	userinfo = ""
	if u.User != nil {
		userinfo = u.User.Username()
	}
	host = u.Hostname()
	port, _ = strconv.Atoi(u.Port())
	if port == 0 {
		port = 443
	}
	return scheme, userinfo, host, port, true
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func decayHealthCounters(health *NodeHealth) bool {
	if health == nil ||
		(health.SuccessCount <= 1000 && health.FailCount <= 200 && health.RecentUseCount <= 500) {
		return false
	}
	health.SuccessCount /= 2
	health.FailCount /= 2
	health.RecentUseCount /= 2
	return true
}

func RecordTest(uri string, ok bool, ms float64, errStr string) {
	mu.Lock()
	defer mu.Unlock()
	ensureLoaded()
	h, exists := healthMap[uri]
	if !exists {
		h = &NodeHealth{} //nolint:exhaustruct
		healthMap[uri] = h
	}
	h.LastTestMs = ms
	h.LastTestError = errStr
	if ok {
		h.SuccessCount++
		h.ConsecutiveFailures = 0
		h.LastSuccessAt = time.Now().Unix()
		h.CooldownUntil = 0
		h.Last429At = 0
		h.RateLimitCount = 0
	} else {
		h.FailCount++
		h.ConsecutiveFailures++
		h.LastFailAt = time.Now().Unix()
		failures := maxInt(1, h.ConsecutiveFailures)
		cooldown := minInt(1800, 30*(1<<minInt(failures-1, 6)))
		h.CooldownUntil = time.Now().Unix() + int64(cooldown)
	}
	decayHealthCounters(h)
	updateSingleNodeHealthUnsafe(uri, h)
}

func UpdateNodeTestResult(uri string, ok bool, ms float64, errStr string) {
	RecordTest(uri, ok, ms, errStr)
}

// RecordRateLimit 记录 429 冷却并递增计次，使重复 429 节点自然降权
func RecordRateLimit(uri string, cooldownSec int) {
	mu.Lock()
	defer mu.Unlock()
	ensureLoaded()
	h, exists := healthMap[uri]
	if !exists {
		h = &NodeHealth{} //nolint:exhaustruct
		healthMap[uri] = h
	}
	now := time.Now().Unix()
	h.CooldownUntil = now + int64(cooldownSec)
	h.Last429At = now
	h.RateLimitCount++
	h.LastTestError = "429 Rate Limit"
	h.LastFailAt = now
	decayHealthCounters(h)
	updateSingleNodeHealthUnsafe(uri, h)
}

// RecordSelection 仅在候选实际启动时记录使用，未被接力启动的候选不会被错误降权。
func RecordSelection(uri string) {
	mu.Lock()
	defer mu.Unlock()
	ensureLoaded()
	if !containsNodeForUpdateUnsafe(uri) {
		return
	}
	health := healthMap[uri]
	if health == nil {
		health = &NodeHealth{} //nolint:exhaustruct
		healthMap[uri] = health
	}
	health.LastSelectedAt = time.Now().Unix()
	health.RecentUseCount++
	decayHealthCounters(health)
	updateSingleNodeHealthUnsafe(uri, health)
}

type scoredNode struct {
	node       Node
	score      float64
	last429    int64
	recovering bool
}

// retainHighestScored 以固定容量最小堆保留最高分节点。调用方最终排序前无需
// 关心堆内顺序；空间复杂度从 O(全部节点) 降为 O(topK)。
func retainHighestScored(nodes []scoredNode, candidate scoredNode, limit int) []scoredNode {
	if limit <= 0 {
		return nodes
	}
	if len(nodes) < limit {
		nodes = append(nodes, candidate)
		if len(nodes) == limit {
			for index := len(nodes)/2 - 1; index >= 0; index-- {
				siftDownScoredMinHeap(nodes, index)
			}
		}
		return nodes
	}
	if candidate.score <= nodes[0].score {
		return nodes
	}
	nodes[0] = candidate
	siftDownScoredMinHeap(nodes, 0)
	return nodes
}

func siftDownScoredMinHeap(nodes []scoredNode, index int) {
	for {
		left := index*2 + 1
		if left >= len(nodes) {
			return
		}
		smallest := left
		if right := left + 1; right < len(nodes) && nodes[right].score < nodes[left].score {
			smallest = right
		}
		if nodes[index].score <= nodes[smallest].score {
			return
		}
		nodes[index], nodes[smallest] = nodes[smallest], nodes[index]
		index = smallest
	}
}

// retainHighestKnown 保留最多 limit 个已验证节点。健康节点始终优先于恢复中
// 节点，同一类别内再按分数选择；这样单遍扫描即可得到与原先双遍计数相同的集合。
func retainHighestKnown(nodes []scoredNode, candidate scoredNode, limit int) []scoredNode {
	if limit <= 0 {
		return nodes
	}
	if len(nodes) < limit {
		nodes = append(nodes, candidate)
		if len(nodes) == limit {
			for index := len(nodes)/2 - 1; index >= 0; index-- {
				siftDownKnownMinHeap(nodes, index)
			}
		}
		return nodes
	}
	if !knownNodeBetter(candidate, nodes[0]) {
		return nodes
	}
	nodes[0] = candidate
	siftDownKnownMinHeap(nodes, 0)
	return nodes
}

func knownNodeBetter(left, right scoredNode) bool {
	if left.recovering != right.recovering {
		return !left.recovering
	}
	return left.score > right.score
}

func siftDownKnownMinHeap(nodes []scoredNode, index int) {
	for {
		left := index*2 + 1
		if left >= len(nodes) {
			return
		}
		worst := left
		if right := left + 1; right < len(nodes) && knownNodeBetter(nodes[left], nodes[right]) {
			worst = right
		}
		if !knownNodeBetter(nodes[index], nodes[worst]) {
			return
		}
		nodes[index], nodes[worst] = nodes[worst], nodes[index]
		index = worst
	}
}

func cooldownEarlier(left, right scoredNode) bool {
	if left.last429 != right.last429 {
		return left.last429 < right.last429
	}
	return left.score < right.score
}

// retainEarliestCooldown 用固定容量最大堆保留最早可重试的冷却节点。
func retainEarliestCooldown(nodes []scoredNode, candidate scoredNode, limit int) []scoredNode {
	if limit <= 0 {
		return nodes
	}
	if len(nodes) < limit {
		nodes = append(nodes, candidate)
		if len(nodes) == limit {
			for index := len(nodes)/2 - 1; index >= 0; index-- {
				siftDownCooldownMaxHeap(nodes, index)
			}
		}
		return nodes
	}
	if !cooldownEarlier(candidate, nodes[0]) {
		return nodes
	}
	nodes[0] = candidate
	siftDownCooldownMaxHeap(nodes, 0)
	return nodes
}

func siftDownCooldownMaxHeap(nodes []scoredNode, index int) {
	for {
		left := index*2 + 1
		if left >= len(nodes) {
			return
		}
		latest := left
		if right := left + 1; right < len(nodes) && cooldownEarlier(nodes[left], nodes[right]) {
			latest = right
		}
		if !cooldownEarlier(nodes[index], nodes[latest]) {
			return
		}
		nodes[index], nodes[latest] = nodes[latest], nodes[index]
		index = latest
	}
}

type NodePoolStats struct {
	Total     int `json:"total"`
	Enabled   int `json:"enabled"`
	Disabled  int `json:"disabled"`
	Healthy   int `json:"healthy"`
	Unhealthy int `json:"unhealthy"`
	Cooling   int `json:"cooling"`
	Untested  int `json:"untested"`
}

func GetNodePoolStats(now time.Time) NodePoolStats {
	lockLoadedForRead()
	defer mu.RUnlock()
	stats := NodePoolStats{Total: len(nodeList)}
	nowUnix := now.Unix()
	for _, node := range nodeList {
		if node.Disabled {
			stats.Disabled++
			continue
		}
		stats.Enabled++
		health := healthMap[node.RawURI]
		switch {
		case health == nil || (health.LastSuccessAt == 0 && health.LastFailAt == 0):
			stats.Untested++
		case health.CooldownUntil > nowUnix:
			stats.Cooling++
		case health.ConsecutiveFailures > 0:
			stats.Unhealthy++
		default:
			stats.Healthy++
		}
	}
	return stats
}

// SelectNodesForHealthCheck 按“未测试、冷却到期失败、健康记录过期”的顺序选择巡检节点。
func SelectNodesForHealthCheck(limit int, staleAfter time.Duration, now time.Time) []Node {
	if limit <= 0 {
		return nil
	}
	lockLoadedForRead()

	type candidate struct {
		node     Node
		priority int
		lastSeen int64
	}
	nowUnix := now.Unix()
	staleBefore := now.Add(-staleAfter).Unix()
	candidates := make([]candidate, 0, min(limit*2, len(nodeList)))
	for _, node := range nodeList {
		if node.Disabled {
			continue
		}
		health := healthMap[node.RawURI]
		switch {
		case health == nil || (health.LastSuccessAt == 0 && health.LastFailAt == 0):
			candidates = append(candidates, candidate{node: node, priority: 0})
		case health.CooldownUntil > nowUnix:
			continue
		case health.ConsecutiveFailures > 0:
			candidates = append(candidates, candidate{
				node: node, priority: 1, lastSeen: health.LastFailAt,
			})
		case staleAfter <= 0 || health.LastSuccessAt <= staleBefore:
			candidates = append(candidates, candidate{
				node: node, priority: 2, lastSeen: health.LastSuccessAt,
			})
		}
	}
	mu.RUnlock()
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].priority != candidates[j].priority {
			return candidates[i].priority < candidates[j].priority
		}
		return candidates[i].lastSeen < candidates[j].lastSeen
	})
	if len(candidates) > limit {
		candidates = candidates[:limit]
	}
	selected := make([]Node, len(candidates))
	for i := range candidates {
		selected[i] = candidates[i].node
	}
	return selected
}

func SelectForParallel(k int, topK int, debugMode bool, stickyBonusEnabled bool) []Node {
	if k <= 0 {
		return nil
	}
	if topK <= 0 {
		topK = 80
	}
	var stickyNodes map[string]struct{}
	if stickyBonusEnabled {
		stickyNodes = globalStickyPool.snapshot()
	}
	now := time.Now().Unix()

	lockLoadedForRead()
	// 默认 topK=80 是请求热路径。单遍扫描用固定容量堆同时保留最高分
	// 已验证节点、少量探索节点和最早冷却节点，避免为精确预分配先扫描一次全池。
	const inlineScoredNodeLimit = 80
	const inlineAuxiliaryNodeLimit = 16
	var inlineKnown [inlineScoredNodeLimit]scoredNode
	var inlineUntested [inlineAuxiliaryNodeLimit]scoredNode
	var inlineCooldown [inlineAuxiliaryNodeLimit]scoredNode
	knownLimit := min(topK, len(nodeList))
	auxiliaryLimit := min(k, len(nodeList))
	var known []scoredNode
	var untested []scoredNode
	var cooldownNodes []scoredNode
	seenUntested := 0
	for _, n := range nodeList {
		if n.Disabled {
			continue
		}
		h := healthMap[n.RawURI]
		if h != nil && h.CooldownUntil > now {
			if cooldownNodes == nil {
				if auxiliaryLimit <= len(inlineCooldown) {
					cooldownNodes = inlineCooldown[:0:auxiliaryLimit]
				} else {
					cooldownNodes = make([]scoredNode, 0, auxiliaryLimit)
				}
			}
			cooldownNodes = retainEarliestCooldown(cooldownNodes, scoredNode{
				node: n, score: float64(h.CooldownUntil), last429: h.Last429At,
			}, auxiliaryLimit)
			continue
		}
		score := 100.0
		if h == nil || (h.LastSuccessAt == 0 && h.LastFailAt == 0) {
			// 未测试节点只占少量探索名额，不能压过已经验证可用的节点。
			if untested == nil {
				if auxiliaryLimit <= len(inlineUntested) {
					untested = inlineUntested[:0:auxiliaryLimit]
				} else {
					untested = make([]scoredNode, 0, auxiliaryLimit)
				}
			}
			seenUntested++
			item := scoredNode{node: n, score: 80}
			if len(untested) < auxiliaryLimit {
				untested = append(untested, item)
			} else if auxiliaryLimit > 0 {
				if index := rand.Intn(seenUntested); index < auxiliaryLimit {
					untested[index] = item
				}
			}
			continue
		}
		if h != nil {
			score += math.Min(float64(h.SuccessCount), 100) * 3
			score -= math.Min(float64(h.FailCount), 100) * 4
			score -= float64(h.ConsecutiveFailures) * 25
			if h.LastTestMs > 0 {
				score -= math.Min(h.LastTestMs/1000.0, 30.0)
			}
			lastSeen := maxInt64(h.LastSuccessAt, h.LastFailAt)
			if now-lastSeen > 3600 {
				score += 10
			}
			score -= math.Min(float64(h.RateLimitCount), 10) * 5
			score -= float64(h.RecentUseCount) * 2
			if h.LastSelectedAt > 0 {
				elapsed := now - h.LastSelectedAt
				if elapsed > 600 {
					score += math.Min(float64(elapsed)/60, 15)
				}
			}
		}
		if _, sticky := stickyNodes[n.RawURI]; sticky {
			score += 40
		}
		if known == nil {
			if knownLimit <= len(inlineKnown) {
				known = inlineKnown[:0:knownLimit]
			} else {
				known = make([]scoredNode, 0, knownLimit)
			}
		}
		item := scoredNode{
			node: n, score: math.Max(1.0, score), recovering: h.ConsecutiveFailures > 0,
		}
		known = retainHighestKnown(known, item, knownLimit)
	}
	mu.RUnlock()

	if len(known) == 0 && len(untested) == 0 && len(cooldownNodes) > 0 {
		slices.SortFunc(cooldownNodes, func(left, right scoredNode) int {
			if left.last429 < right.last429 {
				return -1
			}
			if left.last429 > right.last429 {
				return 1
			}
			if left.score < right.score {
				return -1
			}
			if left.score > right.score {
				return 1
			}
			return 0
		})
		needed := k
		if needed > len(cooldownNodes) {
			needed = len(cooldownNodes)
		}
		selected := make([]Node, needed)
		for i := 0; i < needed; i++ {
			selected[i] = cooldownNodes[i].node
		}
		if debugMode {
			log.Printf("[Nodes] 所有节点冷却中，按 Last429At 兜底选择 %d 个", len(selected))
		}
		return selected
	}

	// 固定容量堆是混合顺序；原地分区后分别排序，恢复健康优先、恢复中兜底语义。
	healthyEnd := 0
	for index := range known {
		if !known[index].recovering {
			known[healthyEnd], known[index] = known[index], known[healthyEnd]
			healthyEnd++
		}
	}
	healthy := known[:healthyEnd]
	recovering := known[healthyEnd:]

	slices.SortFunc(healthy, func(left, right scoredNode) int {
		switch {
		case left.score > right.score:
			return -1
		case left.score < right.score:
			return 1
		default:
			return 0
		}
	})
	slices.SortFunc(recovering, func(left, right scoredNode) int {
		switch {
		case left.score > right.score:
			return -1
		case left.score < right.score:
			return 1
		default:
			return 0
		}
	})
	rand.Shuffle(len(untested), func(i, j int) { untested[i], untested[j] = untested[j], untested[i] })

	if len(healthy) > topK {
		healthy = healthy[:topK]
		recovering = nil
	} else if remaining := topK - len(healthy); len(recovering) > remaining {
		recovering = recovering[:remaining]
	}

	knownCount := len(healthy) + len(recovering)
	exploreTarget := 0
	if knownCount == 0 {
		exploreTarget = min(k, len(untested))
	} else if k >= 5 {
		exploreTarget = min(k/5, len(untested))
	}
	knownTarget := k - min(exploreTarget, len(untested))
	if knownTarget > knownCount {
		knownTarget = knownCount
	}

	knownSelected := weightedNodeSample(healthy, min(knownTarget, len(healthy)))
	if len(knownSelected) < knownTarget {
		knownSelected = append(
			knownSelected,
			weightedNodeSample(recovering, knownTarget-len(knownSelected))...,
		)
	}
	// 没有探索名额且已选满时，knownSelected 本身就是最终顺序，直接返回可
	// 避免再分配并复制一份结果切片。这也是全部节点健康时的常见路径。
	if len(knownSelected) >= k {
		if debugMode {
			log.Printf("[Nodes] 选择并行节点 (需求: %d, 实际: %d)", k, k)
		}
		return knownSelected[:k]
	}
	untestedTarget := min(k-len(knownSelected), len(untested))
	untestedSelected := make([]Node, untestedTarget)
	for i := range untestedSelected {
		untestedSelected[i] = untested[i].node
	}

	// 已验证节点优先；每 4 个已验证节点插入 1 个探索节点。
	selected := make([]Node, 0, min(k, knownCount+len(untested)))
	knownIndex := 0
	untestedIndex := 0
	for len(selected) < k && (knownIndex < len(knownSelected) || untestedIndex < len(untestedSelected)) {
		for range 4 {
			if knownIndex >= len(knownSelected) || len(selected) >= k {
				break
			}
			selected = append(selected, knownSelected[knownIndex])
			knownIndex++
		}
		if untestedIndex < len(untestedSelected) && len(selected) < k {
			selected = append(selected, untestedSelected[untestedIndex])
			untestedIndex++
		}
	}
	for len(selected) < k && knownIndex < len(knownSelected) {
		selected = append(selected, knownSelected[knownIndex])
		knownIndex++
	}
	for len(selected) < k && untestedIndex < len(untested) {
		selected = append(selected, untested[untestedIndex].node)
		untestedIndex++
	}
	if len(selected) < k {
		remainingKnown := append(
			append([]scoredNode(nil), healthy...),
			recovering...,
		)
		alreadySelected := make(map[string]bool, len(selected))
		for _, node := range selected {
			alreadySelected[node.RawURI] = true
		}
		for _, item := range remainingKnown {
			if len(selected) >= k {
				break
			}
			if !alreadySelected[item.node.RawURI] {
				selected = append(selected, item.node)
				alreadySelected[item.node.RawURI] = true
			}
		}
	}

	if debugMode {
		log.Printf("[Nodes] 选择并行节点 (需求: %d, 实际: %d)", k, len(selected))
	}
	return selected
}

func weightedNodeSample(candidates []scoredNode, count int) []Node {
	if count <= 0 || len(candidates) == 0 {
		return nil
	}
	type weightedCandidate struct {
		index  int
		weight float64
	}
	const inlineCandidateLimit = 80
	var inlineCandidates [inlineCandidateLimit]weightedCandidate
	pool := inlineCandidates[:0]
	if len(candidates) <= len(inlineCandidates) {
		pool = inlineCandidates[:len(candidates)]
	} else {
		pool = make([]weightedCandidate, len(candidates))
	}
	totalWeight := 0.0
	const tau = 40.0
	for index, candidate := range candidates {
		weight := math.Exp(candidate.score / tau)
		if math.IsInf(weight, 0) || math.IsNaN(weight) {
			weight = 1
		}
		pool[index] = weightedCandidate{index: index, weight: weight}
		totalWeight += weight
	}
	if count > len(candidates) {
		count = len(candidates)
	}
	selected := make([]Node, 0, count)
	for len(selected) < count {
		pick := rand.Float64() * totalWeight
		index := len(pool) - 1
		for i, candidate := range pool {
			pick -= candidate.weight
			if pick <= 0 {
				index = i
				break
			}
		}
		selected = append(selected, candidates[pool[index].index].node)
		totalWeight -= pool[index].weight
		pool = append(pool[:index], pool[index+1:]...)
	}
	return selected
}

func GetAverageLatency() float64 {
	lockLoadedForRead()
	defer mu.RUnlock()
	var sum float64
	var count int
	now := time.Now().Unix()
	for _, n := range nodeList {
		if n.Disabled {
			continue
		}
		h := healthMap[n.RawURI]
		if h != nil && h.LastTestMs > 0 && h.CooldownUntil <= now {
			sum += h.LastTestMs
			count++
		}
	}
	if count == 0 {
		return 500.0
	}
	return sum / float64(count)
}
