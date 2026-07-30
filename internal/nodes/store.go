package nodes

import (
	"context"
	"database/sql"
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

	"github.com/bsfdsagfadg/vertex/internal/base64x"
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
	healthByNodeIndex   []*NodeHealth                     //nolint:gochecknoglobals
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
	healthMap = loadedHealth
	rebuildNodeIndexUnsafe()
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
	rebuildNodeHealthIndexUnsafe()
}

func rebuildNodeHealthIndexUnsafe() {
	healthIndex := make([]*NodeHealth, len(nodeList))
	for position := range nodeList {
		healthIndex[position] = healthMap[nodeList[position].RawURI]
	}
	healthByNodeIndex = healthIndex
}

func storeNodeHealthUnsafe(uri string, health *NodeHealth) {
	healthMap[uri] = health
	position, exists := nodeIndexByURI[uri]
	if !exists || position < 0 || position >= len(nodeList) || nodeList[position].RawURI != uri {
		return
	}
	if len(healthByNodeIndex) != len(nodeList) {
		rebuildNodeHealthIndexUnsafe()
		return
	}
	healthByNodeIndex[position] = health
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

func nodeIndexForUpdateUnsafe(uri string) (int, bool) {
	if position, ok := nodeIndexByURI[uri]; ok &&
		position >= 0 && position < len(nodeList) && nodeList[position].RawURI == uri {
		return position, true
	}
	// 列表增删/排序后索引可能暂时陈旧；写路径发现 miss 时重建一次，
	// 后续候选启动与名称查询恢复 O(1)。
	rebuildNodeIndexUnsafe()
	position, ok := nodeIndexByURI[uri]
	return position, ok
}

func containsNodeForUpdateUnsafe(uri string) bool {
	_, found := nodeIndexForUpdateUnsafe(uri)
	return found
}

func LoadNodes() []Node {
	lockLoadedForRead()
	defer mu.RUnlock()
	out := append([]Node(nil), nodeList...)
	for i := range out {
		out[i].SubscriptionSourceCount = len(subscriptionSources[out[i].RawURI])
	}
	return out
}

func LoadHealth() map[string]*NodeHealth {
	lockLoadedForRead()
	defer mu.RUnlock()
	return cloneHealthMapUnsafe()
}

type NodePoolSnapshot struct {
	Nodes     []Node
	Health    []NodeHealth
	HasHealth []bool
	Stats     NodePoolStats
}

// NodePoolPageSnapshot is a detached, internally consistent filtered page.
// TotalMatches and pagination metadata describe the complete filtered result,
// while Nodes, Health and HasHealth contain only the selected page.
type NodePoolPageSnapshot struct {
	Nodes        []Node
	Health       []NodeHealth
	HasHealth    []bool
	Stats        NodePoolStats
	TotalMatches int
	Page         int
	PageSize     int
	TotalPages   int
}

// LoadNodePoolSnapshot returns a detached, internally consistent view for
// callers that need nodes, health and aggregate stats together.
func LoadNodePoolSnapshot(now time.Time) NodePoolSnapshot {
	lockLoadedForRead()
	defer mu.RUnlock()

	snapshot := NodePoolSnapshot{
		Nodes:     append([]Node(nil), nodeList...),
		Health:    make([]NodeHealth, len(nodeList)),
		HasHealth: make([]bool, len(nodeList)),
		Stats:     NodePoolStats{Total: len(nodeList)},
	}
	nowUnix := now.Unix()
	indexedHealth := len(healthByNodeIndex) == len(nodeList)
	for index := range snapshot.Nodes {
		node := snapshot.Nodes[index]
		snapshot.Nodes[index].SubscriptionSourceCount = len(subscriptionSources[node.RawURI])
		var health *NodeHealth
		if indexedHealth {
			health = healthByNodeIndex[index]
		} else {
			health = healthMap[node.RawURI]
		}
		if health != nil {
			snapshot.Health[index] = *health
			snapshot.HasHealth[index] = true
		}
		addNodePoolStats(&snapshot.Stats, node, health, nowUnix)
	}
	return snapshot
}

// LoadFilteredNodeURIs returns a detached list of matching node URIs without
// materializing node and health snapshots. match runs under the node read lock,
// must be side-effect free and must not retain the health pointer.
func LoadFilteredNodeURIs(match func(Node, *NodeHealth) bool) []string {
	lockLoadedForRead()
	defer mu.RUnlock()

	uris := make([]string, 0, len(nodeList))
	indexedHealth := len(healthByNodeIndex) == len(nodeList)
	for index, storedNode := range nodeList {
		var health *NodeHealth
		if indexedHealth {
			health = healthByNodeIndex[index]
		} else {
			health = healthMap[storedNode.RawURI]
		}
		node := storedNode
		node.SubscriptionSourceCount = len(subscriptionSources[node.RawURI])
		if match == nil || match(node, health) {
			uris = append(uris, node.RawURI)
		}
	}
	return uris
}

// LoadNodePoolPageSnapshot filters and pages the node pool while holding one
// read lock. match must be side-effect free and must not retain the health
// pointer. A non-positive pageSize preserves the unpaged behavior and returns
// every match on page one.
func LoadNodePoolPageSnapshot(
	now time.Time,
	page int,
	pageSize int,
	match func(Node, *NodeHealth) bool,
) NodePoolPageSnapshot {
	lockLoadedForRead()
	defer mu.RUnlock()

	if page < 1 {
		page = 1
	}
	requestedPage := page
	unpaged := pageSize < 1
	if unpaged {
		page = 1
		requestedPage = 1
	}

	snapshot := NodePoolPageSnapshot{
		Stats: NodePoolStats{Total: len(nodeList)},
		Page:  page,
	}
	if !unpaged {
		snapshot.PageSize = pageSize
		snapshot.Nodes = make([]Node, 0, min(pageSize, len(nodeList)))
		snapshot.Health = make([]NodeHealth, 0, min(pageSize, len(nodeList)))
		snapshot.HasHealth = make([]bool, 0, min(pageSize, len(nodeList)))
	} else {
		snapshot.Nodes = make([]Node, 0, len(nodeList))
		snapshot.Health = make([]NodeHealth, 0, len(nodeList))
		snapshot.HasHealth = make([]bool, 0, len(nodeList))
	}

	start := 0
	end := len(nodeList)
	if !unpaged {
		start, end = nodePoolPageBounds(len(nodeList), requestedPage, pageSize)
	}
	nowUnix := now.Unix()
	indexedHealth := len(healthByNodeIndex) == len(nodeList)
	for index, storedNode := range nodeList {
		var health *NodeHealth
		if indexedHealth {
			health = healthByNodeIndex[index]
		} else {
			health = healthMap[storedNode.RawURI]
		}
		addNodePoolStats(&snapshot.Stats, storedNode, health, nowUnix)

		node := storedNode
		node.SubscriptionSourceCount = len(subscriptionSources[node.RawURI])
		if match != nil && !match(node, health) {
			continue
		}
		matchedIndex := snapshot.TotalMatches
		snapshot.TotalMatches++
		if matchedIndex < start || matchedIndex >= end {
			continue
		}
		appendNodePoolPageEntry(&snapshot, node, health)
	}

	if unpaged {
		snapshot.PageSize = max(1, snapshot.TotalMatches)
		snapshot.TotalPages = 1
		return snapshot
	}

	snapshot.TotalPages = 1
	if snapshot.TotalMatches > 0 {
		snapshot.TotalPages = (snapshot.TotalMatches-1)/pageSize + 1
	}
	if requestedPage <= snapshot.TotalPages {
		return snapshot
	}

	// Preserve the admin API's historical page clamping without retaining all
	// matching nodes: only an out-of-range request needs this second scan.
	snapshot.Page = snapshot.TotalPages
	start, end = nodePoolPageBounds(snapshot.TotalMatches, snapshot.Page, pageSize)
	snapshot.Nodes = snapshot.Nodes[:0]
	snapshot.Health = snapshot.Health[:0]
	snapshot.HasHealth = snapshot.HasHealth[:0]
	matchedIndex := 0
	for index, storedNode := range nodeList {
		var health *NodeHealth
		if indexedHealth {
			health = healthByNodeIndex[index]
		} else {
			health = healthMap[storedNode.RawURI]
		}
		node := storedNode
		node.SubscriptionSourceCount = len(subscriptionSources[node.RawURI])
		if match != nil && !match(node, health) {
			continue
		}
		if matchedIndex >= start && matchedIndex < end {
			appendNodePoolPageEntry(&snapshot, node, health)
		}
		matchedIndex++
		if matchedIndex >= end {
			break
		}
	}
	return snapshot
}

func nodePoolPageBounds(total, page, pageSize int) (int, int) {
	if total <= 0 || page < 1 || pageSize < 1 {
		return 0, max(0, total)
	}
	pageIndex := page - 1
	if pageIndex > total/pageSize {
		return total, total
	}
	start := pageIndex * pageSize
	if start >= total {
		return total, total
	}
	if pageSize >= total-start {
		return start, total
	}
	return start, start + pageSize
}

func appendNodePoolPageEntry(snapshot *NodePoolPageSnapshot, node Node, health *NodeHealth) {
	snapshot.Nodes = append(snapshot.Nodes, node)
	if health == nil {
		snapshot.Health = append(snapshot.Health, NodeHealth{})
		snapshot.HasHealth = append(snapshot.HasHealth, false)
		return
	}
	snapshot.Health = append(snapshot.Health, *health)
	snapshot.HasHealth = append(snapshot.HasHealth, true)
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

func updateSingleNodeHealthUnsafe(uri string, h *NodeHealth) {
	database := db.CurrentDB()
	if database == nil || h == nil {
		return
	}
	healthOnce.Do(initHealthQueue)
	enqueueHealthUpdate(healthUpdate{database: database, uri: uri, h: *h})
}

func updateSingleNodeDisabledWithErrorUnsafe(uri string, disabled bool) error {
	database := db.CurrentDB()
	if database == nil {
		return nil
	}
	_, err := database.Exec("UPDATE nodes SET disabled = ? WHERE raw_uri = ?", disabled, uri)
	return err
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

// pruneHealthUnsafe removes orphaned health rows after nodeIndexByURI has been
// rebuilt for the current nodeList and returns the removed entries for callers
// that may need to roll the change back.
func pruneHealthUnsafe() map[string]*NodeHealth {
	var removed map[string]*NodeHealth
	estimatedOrphans := max(len(healthMap)-len(nodeIndexByURI), 1)
	for uri, health := range healthMap {
		if _, found := nodeIndexByURI[uri]; !found {
			if removed == nil {
				removed = make(map[string]*NodeHealth, estimatedOrphans)
			}
			removed[uri] = health
			delete(healthMap, uri)
		}
	}
	return removed
}

func restoreSubscriptionMembershipUnsafe(sourceID int64, previous []string) {
	for rawURI, sourceIDs := range subscriptionSources {
		if !sourceIDs[sourceID] {
			continue
		}
		delete(sourceIDs, sourceID)
		if len(sourceIDs) == 0 {
			delete(subscriptionSources, rawURI)
		}
	}
	for _, rawURI := range previous {
		if subscriptionSources[rawURI] == nil {
			subscriptionSources[rawURI] = make(map[int64]bool)
		}
		subscriptionSources[rawURI][sourceID] = true
	}
}

type detachedNodeRuntimeState struct {
	sourceIDs  map[int64]bool
	health     *NodeHealth
	hadSources bool
	hadHealth  bool
}

type removedNodePosition struct {
	originalIndex int
	node          Node
	runtimeState  detachedNodeRuntimeState
}

// restoreRemovedNodePositionsUnsafe reconstructs the original list after it
// has been compacted in place. Walking backwards keeps unread compacted nodes
// from being overwritten while removed nodes are inserted at their old slots.
func restoreRemovedNodePositionsUnsafe(removed []removedNodePosition) {
	keptCount := len(nodeList)
	originalCount := keptCount + len(removed)
	nodeList = nodeList[:originalCount]
	keptIndex := keptCount - 1
	removedIndex := len(removed) - 1
	for originalIndex := originalCount - 1; originalIndex >= 0; originalIndex-- {
		if removedIndex >= 0 && removed[removedIndex].originalIndex == originalIndex {
			nodeList[originalIndex] = removed[removedIndex].node
			removedIndex--
			continue
		}
		nodeList[originalIndex] = nodeList[keptIndex]
		keptIndex--
	}
}

func publishRemovedNodeIndexesUnsafe(removed []removedNodePosition) {
	for _, item := range removed {
		delete(nodeIndexByURI, item.node.RawURI)
	}
	for index, node := range nodeList {
		nodeIndexByURI[node.RawURI] = index
	}
	rebuildNodeHealthIndexUnsafe()
}

func detachNodeRuntimeStateUnsafe(rawURI string) detachedNodeRuntimeState {
	sourceIDs, hadSources := subscriptionSources[rawURI]
	health, hadHealth := healthMap[rawURI]
	delete(subscriptionSources, rawURI)
	delete(healthMap, rawURI)
	return detachedNodeRuntimeState{
		sourceIDs:  sourceIDs,
		health:     health,
		hadSources: hadSources,
		hadHealth:  hadHealth,
	}
}

func restoreNodeRuntimeStateUnsafe(
	rawURI string,
	state detachedNodeRuntimeState,
) {
	if state.hadSources {
		subscriptionSources[rawURI] = state.sourceIDs
	}
	if state.hadHealth {
		healthMap[rawURI] = state.health
	}
}

func restoreNodeRuntimeStatesUnsafe(states map[string]detachedNodeRuntimeState) {
	for rawURI, state := range states {
		restoreNodeRuntimeStateUnsafe(rawURI, state)
	}
}

type subscriptionSourceState struct {
	sourceIDs map[int64]bool
	hadValue  bool
}

func snapshotSubscriptionSourceUnsafe(rawURI string) subscriptionSourceState {
	sourceIDs, hadValue := subscriptionSources[rawURI]
	if !hadValue {
		return subscriptionSourceState{}
	}
	copied := make(map[int64]bool, len(sourceIDs))
	for sourceID, present := range sourceIDs {
		copied[sourceID] = present
	}
	return subscriptionSourceState{sourceIDs: copied, hadValue: hadValue}
}

func restoreSubscriptionSourceStatesUnsafe(states map[string]subscriptionSourceState) {
	for rawURI, state := range states {
		if state.hadValue {
			subscriptionSources[rawURI] = state.sourceIDs
		} else {
			delete(subscriptionSources, rawURI)
		}
	}
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
	// nextNodes is built in independent storage, so the current slice can be
	// retained directly as the immutable persistence and rollback snapshot.
	previousNodes := nodeList

	desired := make(map[string]Node, len(newNodes))
	desiredOrder := make([]string, 0, len(newNodes))
	for _, node := range newNodes {
		node.RawURI = strings.TrimSpace(node.RawURI)
		if node.RawURI == "" {
			continue
		}
		node.SourceID = 0
		node.SubscriptionSourceCount = 0
		if _, exists := desired[node.RawURI]; !exists {
			desiredOrder = append(desiredOrder, node.RawURI)
		}
		desired[node.RawURI] = node
	}

	nextNodes := make([]Node, 0, len(nodeList)+len(desired))
	keptExisting := make([]bool, len(nodeList))
	for index, current := range nodeList {
		sources := subscriptionSources[current.RawURI]
		if len(sources) == 0 {
			continue
		}
		if replacement, replace := desired[current.RawURI]; replace {
			replacement.Disabled = current.Disabled
			replacement.SourceID = 0
			nextNodes = append(nextNodes, replacement)
			keptExisting[index] = true
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
		keptExisting[index] = true
	}
	for _, rawURI := range desiredOrder {
		replacement, exists := desired[rawURI]
		if !exists {
			continue
		}
		if index, found := nodeIndexByURI[rawURI]; found {
			replacement.Disabled = nodeList[index].Disabled
			keptExisting[index] = true
		}
		nextNodes = append(nextNodes, replacement)
	}

	removedURIs := make([]string, 0)
	var detachedStates map[string]detachedNodeRuntimeState
	for index, current := range nodeList {
		if !keptExisting[index] {
			removedURIs = append(removedURIs, current.RawURI)
			state := detachNodeRuntimeStateUnsafe(current.RawURI)
			if state.hadSources || state.hadHealth {
				if detachedStates == nil {
					detachedStates = make(map[string]detachedNodeRuntimeState)
				}
				detachedStates[current.RawURI] = state
			}
		}
	}
	nodeList = nextNodes
	if err := persistIndexedNodeReplacementUnsafe(previousNodes, removedURIs); err != nil {
		nodeList = previousNodes
		restoreNodeRuntimeStatesUnsafe(detachedStates)
		mu.Unlock()
		return err
	}
	rebuildNodeIndexUnsafe()
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
	previousNodeCount := len(nodeList)
	inserted := make([]nodeInsert, 0, len(newNodes))
	manualized := make([]nodeManualization, 0)
	for _, n := range newNodes {
		n.RawURI = strings.TrimSpace(n.RawURI)
		if n.RawURI == "" {
			continue
		}
		n.SourceID = 0
		n.SubscriptionSourceCount = 0
		index, found := nodeIndexByURI[n.RawURI]
		if found {
			// 手动导入与订阅节点重合时，标记为手动节点；订阅关系仍会保留。
			if nodeList[index].SourceID != 0 {
				manualized = append(manualized, nodeManualization{
					rawURI:           n.RawURI,
					index:            index,
					previousSourceID: nodeList[index].SourceID,
				})
				nodeList[index].SourceID = 0
			}
			continue
		}
		sortOrder := len(nodeList)
		nodeList = append(nodeList, n)
		nodeIndexByURI[n.RawURI] = len(nodeList) - 1
		inserted = append(inserted, nodeInsert{node: n, sortOrder: sortOrder})
	}
	prunedHealth := pruneHealthUnsafe()
	if err := persistMergedNodesUnsafe(inserted, manualized); err != nil {
		nodeList = nodeList[:previousNodeCount]
		for _, change := range inserted {
			delete(nodeIndexByURI, change.node.RawURI)
		}
		for _, change := range manualized {
			nodeList[change.index].SourceID = change.previousSourceID
		}
		for rawURI, health := range prunedHealth {
			healthMap[rawURI] = health
		}
		log.Printf("[Nodes] 合并节点持久化失败，已回滚内存状态: %v", err)
		return err
	}
	rebuildNodeHealthIndexUnsafe()
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
	if db.CurrentDB() == nil {
		mu.Unlock()
		return SubscriptionNodeSyncResult{}, errors.New("database unavailable")
	}
	finishUnchanged := func(count int) (SubscriptionNodeSyncResult, error) {
		result := SubscriptionNodeSyncResult{Count: count}
		var persistFinalize func(*sql.Tx) error
		if finalize != nil {
			persistFinalize = func(tx *sql.Tx) error {
				return finalize(tx, result)
			}
		}
		err := persistSubscriptionSyncUnsafe(subscriptionSyncChanges{
			sourceID: sourceID,
			count:    result.Count,
		}, persistFinalize)
		mu.Unlock()
		if err != nil {
			return SubscriptionNodeSyncResult{}, err
		}
		return result, nil
	}
	if subscriptionSyncInputUnchangedUnsafe(sourceID, newNodes) {
		return finishUnchanged(len(newNodes))
	}

	desired, desiredSet := normalizeSubscriptionNodesUnsafe(newNodes)
	if subscriptionSyncUnchangedUnsafe(sourceID, desired) {
		return finishUnchanged(len(desired))
	}

	previousMembership := make([]string, 0, len(desired))
	for rawURI, sourceIDs := range subscriptionSources {
		if sourceIDs[sourceID] {
			previousMembership = append(previousMembership, rawURI)
		}
	}
	originalNodeCount := len(nodeList)
	var previousNodeValues map[string]Node
	capturePreviousNode := func(node Node) {
		if previousNodeValues == nil {
			previousNodeValues = make(map[string]Node)
		}
		if _, captured := previousNodeValues[node.RawURI]; !captured {
			previousNodeValues[node.RawURI] = node
		}
	}
	result := SubscriptionNodeSyncResult{}

	insertedNodes := make(map[string]Node)
	updatedNodes := make(map[string]Node)
	membershipsAdded := make([]string, 0)
	for _, desiredNode := range desired {
		next := desiredNode
		uri := next.RawURI
		wasMember := subscriptionSources[uri][sourceID]
		if subscriptionSources[uri] == nil {
			subscriptionSources[uri] = make(map[int64]bool)
		}
		subscriptionSources[uri][sourceID] = true
		if !wasMember {
			membershipsAdded = append(membershipsAdded, uri)
		}
		if index, exists := nodeIndexByURI[uri]; exists {
			current := nodeList[index]
			if current.SourceID != 0 && (current.SourceID == sourceID || sourceID < current.SourceID) {
				next.SourceID = sourceID
				next.Disabled = current.Disabled
				if strings.TrimSpace(next.Name) == "" {
					next.Name = current.Name
				}
				if next != current {
					capturePreviousNode(current)
					nodeList[index] = next
					updatedNodes[uri] = next
				}
			}
		} else {
			next.SourceID = sourceID
			nodeList = append(nodeList, next)
			insertedNodes[uri] = next
			result.Added++
		}
		result.Count++
	}

	membershipsRemoved := make([]string, 0)
	for _, uri := range previousMembership {
		if desiredSet.containsUnsafe(uri) {
			continue
		}
		delete(subscriptionSources[uri], sourceID)
		if len(subscriptionSources[uri]) == 0 {
			delete(subscriptionSources, uri)
		}
		membershipsRemoved = append(membershipsRemoved, uri)
	}

	var removedNodes []removedNodePosition
	keptCount := 0
	var removedURIs []string
	var removedHealth map[string]*NodeHealth
	for originalIndex, n := range nodeList {
		original := n
		sources := subscriptionSources[n.RawURI]
		if n.SourceID == 0 {
			nodeList[keptCount] = n
			keptCount++
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
			if n != original {
				if _, existed := nodeIndexByURI[n.RawURI]; existed {
					capturePreviousNode(original)
				}
				updatedNodes[n.RawURI] = n
			}
			nodeList[keptCount] = n
			keptCount++
			continue
		}

		result.Removed++
		removedURIs = append(removedURIs, n.RawURI)
		removedNodes = append(removedNodes, removedNodePosition{
			originalIndex: originalIndex,
			node:          n,
		})
		delete(subscriptionSources, n.RawURI)
		if health, exists := healthMap[n.RawURI]; exists {
			if removedHealth == nil {
				removedHealth = make(map[string]*NodeHealth)
			}
			removedHealth[n.RawURI] = health
		}
		delete(healthMap, n.RawURI)
		delete(updatedNodes, n.RawURI)
	}
	nodeList = nodeList[:keptCount]
	inserted := make([]nodeInsert, 0, len(insertedNodes))
	updated := make([]Node, 0, len(updatedNodes))
	positionChanges := make([]nodeInsert, 0)
	for index, node := range nodeList {
		if _, ok := insertedNodes[node.RawURI]; ok {
			inserted = append(inserted, nodeInsert{node: node, sortOrder: index})
		} else if previousIndex, existed := nodeIndexByURI[node.RawURI]; existed && previousIndex != index {
			positionChanges = append(positionChanges, nodeInsert{node: node, sortOrder: index})
		}
		if _, ok := updatedNodes[node.RawURI]; ok {
			updated = append(updated, node)
		}
	}
	var persistFinalize func(*sql.Tx) error
	if finalize != nil {
		persistFinalize = func(tx *sql.Tx) error {
			return finalize(tx, result)
		}
	}
	changes := subscriptionSyncChanges{
		sourceID:           sourceID,
		count:              result.Count,
		inserted:           inserted,
		updated:            updated,
		membershipsAdded:   membershipsAdded,
		membershipsRemoved: membershipsRemoved,
		removedNodes:       removedURIs,
		positionChanges:    positionChanges,
	}
	if err := persistSubscriptionSyncUnsafe(changes, persistFinalize); err != nil {
		if len(removedNodes) > 0 {
			restoreRemovedNodePositionsUnsafe(removedNodes)
		}
		nodeList = nodeList[:originalNodeCount]
		for rawURI, previous := range previousNodeValues {
			if index, exists := nodeIndexByURI[rawURI]; exists {
				nodeList[index] = previous
			}
		}
		restoreSubscriptionMembershipUnsafe(sourceID, previousMembership)
		for rawURI, health := range removedHealth {
			healthMap[rawURI] = health
		}
		mu.Unlock()
		return SubscriptionNodeSyncResult{}, err
	}
	for _, rawURI := range removedURIs {
		delete(nodeIndexByURI, rawURI)
	}
	for index, node := range nodeList {
		nodeIndexByURI[node.RawURI] = index
	}
	rebuildNodeHealthIndexUnsafe()
	for _, rawURI := range removedURIs {
		globalStickyPool.Evict(rawURI)
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

// subscriptionSyncInputUnchangedUnsafe handles the common case where a
// subscription refresh already contains normalized, unique nodes. A compact
// index bitmap detects duplicates without copying the input or building a
// full URI map.
func subscriptionSyncInputUnchangedUnsafe(sourceID int64, newNodes []Node) bool {
	membershipCount := 0
	for _, sourceIDs := range subscriptionSources {
		if sourceIDs[sourceID] {
			membershipCount++
		}
	}
	if membershipCount != len(newNodes) {
		return false
	}

	seen := make([]bool, len(nodeList))
	for _, next := range newNodes {
		rawURI := strings.TrimSpace(next.RawURI)
		if rawURI == "" || rawURI != next.RawURI || !subscriptionSources[rawURI][sourceID] {
			return false
		}
		index, exists := nodeIndexByURI[rawURI]
		if !exists || index < 0 || index >= len(nodeList) || seen[index] {
			return false
		}
		seen[index] = true

		current := nodeList[index]
		if current.SourceID == 0 || current.SourceID != sourceID && sourceID >= current.SourceID {
			continue
		}
		next.SubscriptionSourceCount = 0
		next.SourceID = sourceID
		next.Disabled = current.Disabled
		if strings.TrimSpace(next.Name) == "" {
			next.Name = current.Name
		}
		if next != current {
			return false
		}
	}
	return true
}

type subscriptionDesiredSet struct {
	byURI      map[string]int
	existing   []bool
	newRawURIs map[string]struct{}
}

func (desired subscriptionDesiredSet) containsUnsafe(rawURI string) bool {
	if desired.byURI != nil {
		_, exists := desired.byURI[rawURI]
		return exists
	}
	if index, exists := nodeIndexByURI[rawURI]; exists &&
		index >= 0 && index < len(desired.existing) {
		return desired.existing[index]
	}
	_, exists := desired.newRawURIs[rawURI]
	return exists
}

// normalizeSubscriptionNodesUnsafe reuses already normalized, unique input
// directly. Existing-node membership is represented by a compact bitmap and
// only genuinely new URIs need a map. Irregular input falls back to the full
// stable-order, last-value-wins normalization path.
func normalizeSubscriptionNodesUnsafe(newNodes []Node) ([]Node, subscriptionDesiredSet) {
	existing := make([]bool, len(nodeList))
	var newRawURIs map[string]struct{}
	fastPath := true
	for _, node := range newNodes {
		rawURI := strings.TrimSpace(node.RawURI)
		if rawURI == "" || rawURI != node.RawURI || node.SubscriptionSourceCount != 0 {
			fastPath = false
			break
		}
		if index, exists := nodeIndexByURI[rawURI]; exists {
			if index < 0 || index >= len(existing) || existing[index] {
				fastPath = false
				break
			}
			existing[index] = true
			continue
		}
		if newRawURIs == nil {
			newRawURIs = make(map[string]struct{})
		}
		if _, duplicate := newRawURIs[rawURI]; duplicate {
			fastPath = false
			break
		}
		newRawURIs[rawURI] = struct{}{}
	}
	if fastPath {
		return newNodes, subscriptionDesiredSet{
			existing:   existing,
			newRawURIs: newRawURIs,
		}
	}

	desired := make([]Node, 0, len(newNodes))
	byURI := make(map[string]int, len(newNodes))
	for _, node := range newNodes {
		node.RawURI = strings.TrimSpace(node.RawURI)
		if node.RawURI == "" {
			continue
		}
		node.SubscriptionSourceCount = 0
		if index, exists := byURI[node.RawURI]; exists {
			desired[index] = node
			continue
		}
		byURI[node.RawURI] = len(desired)
		desired = append(desired, node)
	}
	return desired, subscriptionDesiredSet{byURI: byURI}
}

func subscriptionSyncUnchangedUnsafe(
	sourceID int64,
	desired []Node,
) bool {
	membershipCount := 0
	for _, sourceIDs := range subscriptionSources {
		if sourceIDs[sourceID] {
			membershipCount++
		}
	}
	if membershipCount != len(desired) {
		return false
	}

	for _, next := range desired {
		rawURI := next.RawURI
		if !subscriptionSources[rawURI][sourceID] {
			return false
		}
		index, exists := nodeIndexByURI[rawURI]
		if !exists {
			return false
		}
		current := nodeList[index]
		if current.SourceID == 0 || current.SourceID != sourceID && sourceID >= current.SourceID {
			continue
		}
		next.SourceID = sourceID
		next.Disabled = current.Disabled
		if strings.TrimSpace(next.Name) == "" {
			next.Name = current.Name
		}
		if next != current {
			return false
		}
	}
	return true
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
	if _, err := DeleteNodeWithError(uri); err != nil {
		log.Printf("[Nodes] 删除节点持久化失败，已回滚内存状态: %v", err)
	}
}

// DeleteNodeWithError removes a node atomically from memory and persistence.
// The bool reports whether a concrete node existed.
func DeleteNodeWithError(uri string) (bool, error) {
	mu.Lock()
	ensureLoaded()
	index, exists := nodeIndexByURI[uri]
	detachedState := detachNodeRuntimeStateUnsafe(uri)
	affectedSourceIDs := make(map[int64]bool)
	for sourceID := range detachedState.sourceIDs {
		affectedSourceIDs[sourceID] = true
	}
	var removed Node
	if exists {
		removed = nodeList[index]
		copy(nodeList[index:], nodeList[index+1:])
		nodeList = nodeList[:len(nodeList)-1]
	}
	if err := deletePersistedNodesUnsafe([]string{uri}, affectedSourceIDs); err != nil {
		if exists {
			nodeList = append(nodeList, Node{})
			copy(nodeList[index+1:], nodeList[index:])
			nodeList[index] = removed
		}
		restoreNodeRuntimeStateUnsafe(uri, detachedState)
		mu.Unlock()
		return false, err
	}
	if exists {
		delete(nodeIndexByURI, uri)
		for current := index; current < len(nodeList); current++ {
			nodeIndexByURI[nodeList[current].RawURI] = current
		}
		rebuildNodeHealthIndexUnsafe()
	}
	globalStickyPool.Evict(uri)
	cb := DeleteNodeCallback
	mu.Unlock() // 必须先解锁，避免底层的销毁回调查找节点名称时发生死锁
	if cb != nil {
		cb(uri)
	}
	return exists, nil
}

func DedupNodes() int {
	mu.Lock()
	ensureLoaded()
	var previousNodes []Node
	var previousMemberships map[membershipKey]bool
	type parsedDedupKey struct {
		scheme   string
		userinfo string
		host     string
		port     int
	}
	parsedKeepMap := make(map[parsedDedupKey]int, len(nodeList))
	var rawKeepMap map[string]int
	// previousNodes is an independent rollback snapshot, so compact the live
	// list in place instead of allocating another full-size node slice.
	kept := nodeList[:0]
	removed := 0
	var removedURIs []string
	var keptSourceStates map[string]subscriptionSourceState
	var removedStates map[string]detachedNodeRuntimeState
	for _, n := range nodeList {
		keptIndex := 0
		exists := false
		if scheme, userinfo, host, port, ok := parseNodeIdentity(n.RawURI); ok {
			key := parsedDedupKey{
				scheme: scheme, userinfo: userinfo, host: host, port: port,
			}
			keptIndex, exists = parsedKeepMap[key]
			if !exists {
				parsedKeepMap[key] = len(kept)
			}
		} else {
			if rawKeepMap == nil {
				rawKeepMap = make(map[string]int)
			}
			keptIndex, exists = rawKeepMap[n.RawURI]
			if !exists {
				rawKeepMap[n.RawURI] = len(kept)
			}
		}
		if !exists {
			kept = append(kept, n)
		} else {
			if previousMemberships == nil {
				// Before the first duplicate, in-place compaction has only
				// written every node back to its original slot. Capture the
				// rollback snapshot lazily so the no-op path avoids a full copy.
				previousNodes = append([]Node(nil), nodeList...)
				previousMemberships = flattenMemberships(subscriptionSources)
				keptSourceStates = make(map[string]subscriptionSourceState)
				removedStates = make(map[string]detachedNodeRuntimeState)
			}
			keptURI := kept[keptIndex].RawURI
			if _, captured := keptSourceStates[keptURI]; !captured {
				keptSourceStates[keptURI] = snapshotSubscriptionSourceUnsafe(keptURI)
			}
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
			state := detachNodeRuntimeStateUnsafe(n.RawURI)
			if state.hadSources || state.hadHealth {
				removedStates[n.RawURI] = state
			}
		}
	}
	if removed == 0 {
		mu.Unlock()
		return 0
	}
	nodeList = kept
	if err := persistNodeSnapshotDiffUnsafe(previousNodes, previousMemberships); err != nil {
		nodeList = previousNodes
		restoreSubscriptionSourceStatesUnsafe(keptSourceStates)
		restoreNodeRuntimeStatesUnsafe(removedStates)
		mu.Unlock()
		log.Printf("[Nodes] 节点去重持久化失败，已回滚内存状态: %v", err)
		return 0
	}
	rebuildNodeIndexUnsafe()
	for _, rawURI := range removedURIs {
		globalStickyPool.Evict(rawURI)
	}
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
	var removedNodes []removedNodePosition
	var removedURIs []string
	var affectedSourceIDs map[int64]bool
	keptCount := 0
	for originalIndex, node := range nodeList {
		if !node.Disabled {
			nodeList[keptCount] = node
			keptCount++
			continue
		}
		state := detachNodeRuntimeStateUnsafe(node.RawURI)
		removedNodes = append(removedNodes, removedNodePosition{
			originalIndex: originalIndex,
			node:          node,
			runtimeState:  state,
		})
		removedURIs = append(removedURIs, node.RawURI)
		for sourceID := range state.sourceIDs {
			if affectedSourceIDs == nil {
				affectedSourceIDs = make(map[int64]bool)
			}
			affectedSourceIDs[sourceID] = true
		}
	}
	if len(removedNodes) == 0 {
		mu.Unlock()
		return 0
	}
	nodeList = nodeList[:keptCount]
	if err := deletePersistedNodesUnsafe(removedURIs, affectedSourceIDs); err != nil {
		restoreRemovedNodePositionsUnsafe(removedNodes)
		for _, removed := range removedNodes {
			restoreNodeRuntimeStateUnsafe(removed.node.RawURI, removed.runtimeState)
		}
		mu.Unlock()
		log.Printf("[Nodes] 删除禁用节点持久化失败，已回滚内存状态: %v", err)
		return 0
	}
	publishRemovedNodeIndexesUnsafe(removedNodes)
	for _, rawURI := range removedURIs {
		globalStickyPool.Evict(rawURI)
	}
	cb := DeleteNodeCallback
	mu.Unlock()

	if cb != nil {
		for _, u := range removedURIs {
			cb(u)
		}
	}
	return len(removedNodes)
}

func BatchUpdateNodesDisabled(uris []string, disabled bool) error {
	mu.Lock()
	defer mu.Unlock()
	ensureLoaded()
	type disabledChange struct {
		index    int
		previous bool
		rawURI   string
	}
	changes := make([]disabledChange, 0, len(uris))
	for _, u := range uris {
		index, exists := nodeIndexByURI[u]
		if !exists || nodeList[index].Disabled == disabled {
			continue
		}
		changes = append(changes, disabledChange{
			index:    index,
			previous: nodeList[index].Disabled,
			rawURI:   u,
		})
		nodeList[index].Disabled = disabled
	}
	database := db.CurrentDB()
	if database == nil || len(changes) == 0 {
		return nil
	}
	restoreChanges := func() {
		for _, change := range changes {
			nodeList[change.index].Disabled = change.previous
		}
	}
	tx, err := database.Begin()
	if err != nil {
		restoreChanges()
		return fmt.Errorf("开始批量更新节点事务: %w", err)
	}
	rollback := func(updateErr error) error {
		_ = tx.Rollback()
		restoreChanges()
		return fmt.Errorf("批量更新节点: %w", updateErr)
	}
	stmt, err := tx.Prepare("UPDATE nodes SET disabled = ? WHERE raw_uri = ?")
	if err != nil {
		return rollback(err)
	}
	for _, change := range changes {
		if _, err := stmt.Exec(disabled, change.rawURI); err != nil {
			_ = stmt.Close()
			return rollback(err)
		}
	}
	if err := stmt.Close(); err != nil {
		return rollback(err)
	}
	if err := tx.Commit(); err != nil {
		restoreChanges()
		return fmt.Errorf("提交批量更新节点事务: %w", err)
	}
	return nil
}

func BatchDeleteNodes(uris []string) error {
	mu.Lock()
	ensureLoaded()
	targets := make(map[string]bool)
	detachedStates := make(map[string]detachedNodeRuntimeState)
	affectedSourceIDs := make(map[int64]bool)
	for _, u := range uris {
		if targets[u] {
			continue
		}
		targets[u] = true
		state := detachNodeRuntimeStateUnsafe(u)
		if state.hadSources || state.hadHealth {
			detachedStates[u] = state
		}
		for sourceID := range state.sourceIDs {
			affectedSourceIDs[sourceID] = true
		}
	}
	var removedNodes []removedNodePosition
	keptCount := 0
	for originalIndex, node := range nodeList {
		if targets[node.RawURI] {
			removedNodes = append(removedNodes, removedNodePosition{
				originalIndex: originalIndex,
				node:          node,
			})
			continue
		}
		nodeList[keptCount] = node
		keptCount++
	}
	nodeList = nodeList[:keptCount]
	if err := deletePersistedNodesUnsafe(uris, affectedSourceIDs); err != nil {
		restoreRemovedNodePositionsUnsafe(removedNodes)
		restoreNodeRuntimeStatesUnsafe(detachedStates)
		mu.Unlock()
		log.Printf("[Nodes] 批量删除节点持久化失败，已回滚内存状态: %v", err)
		return err
	}
	publishRemovedNodeIndexesUnsafe(removedNodes)
	for _, rawURI := range uris {
		globalStickyPool.Evict(rawURI)
	}
	cb := DeleteNodeCallback
	mu.Unlock() // 防止在批量删除时引发卡死死锁

	if cb != nil {
		for _, u := range uris {
			cb(u)
		}
	}
	return nil
}

func nodeLatencySortValueUnsafe(node Node) float64 {
	health := healthMap[node.RawURI]
	if health == nil {
		return math.MaxFloat64
	}
	if health.ConsecutiveFailures > 0 {
		return 1e6 + float64(health.ConsecutiveFailures)*1000
	}
	if health.LastTestMs > 0 {
		return health.LastTestMs
	}
	return math.MaxFloat64
}

func nodeLatencyLessUnsafe(left, right Node, descending bool) bool {
	// Disabled nodes stay last in both directions.
	if left.Disabled != right.Disabled {
		return !left.Disabled
	}
	leftValue := nodeLatencySortValueUnsafe(left)
	rightValue := nodeLatencySortValueUnsafe(right)
	if leftValue == rightValue {
		return left.Name < right.Name
	}
	if descending {
		return leftValue > rightValue
	}
	return leftValue < rightValue
}

func sortNodesByLatency(descending bool) {
	mu.Lock()
	ensureLoaded()
	less := func(i, j int) bool {
		return nodeLatencyLessUnsafe(nodeList[i], nodeList[j], descending)
	}
	if sort.SliceIsSorted(nodeList, less) {
		mu.Unlock()
		return
	}
	previousNodes := append([]Node(nil), nodeList...)
	sort.Slice(nodeList, less)
	if err := persistNodeOrderUnsafe(); err != nil {
		nodeList = previousNodes
		log.Printf("[Nodes] 保存节点排序失败，已回滚内存状态: %v", err)
	} else {
		for index, node := range nodeList {
			nodeIndexByURI[node.RawURI] = index
		}
		rebuildNodeHealthIndexUnsafe()
	}
	mu.Unlock()
}

func SortNodesByLatency() {
	sortNodesByLatency(false)
}

func SortNodesByLatencyDesc() {
	sortNodesByLatency(true)
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
	enabled, err := EnableNodeWithError(uri)
	if err != nil {
		log.Printf("[Nodes] 启用节点持久化失败，已回滚内存状态: %v", err)
		return false
	}
	return enabled
}

// EnableNodeWithError enables a node only after its durable disabled flag can
// be updated, preventing the admin API from reporting success on DB failure.
func EnableNodeWithError(uri string) (bool, error) {
	mu.Lock()
	defer mu.Unlock()
	ensureLoaded()
	index, found := nodeIndexForUpdateUnsafe(uri)
	if !found {
		return false, nil
	}
	if err := updateSingleNodeDisabledWithErrorUnsafe(uri, false); err != nil {
		return false, err
	}
	nodeList[index].Disabled = false
	if health, exists := healthMap[uri]; exists {
		health.CooldownUntil = 0
		updateSingleNodeHealthUnsafe(uri, health)
	}
	return true, nil
}

type vmessIdentityString string

func (value *vmessIdentityString) UnmarshalJSON(encoded []byte) error {
	if len(encoded) == 0 || encoded[0] != '"' {
		return nil
	}
	var decoded string
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		return err
	}
	*value = vmessIdentityString(decoded)
	return nil
}

type vmessIdentityPort int

func (value *vmessIdentityPort) UnmarshalJSON(encoded []byte) error {
	if len(encoded) == 0 {
		return nil
	}
	if encoded[0] == '"' {
		var decoded string
		if err := json.Unmarshal(encoded, &decoded); err != nil {
			return err
		}
		port, err := strconv.Atoi(decoded)
		if err == nil {
			*value = vmessIdentityPort(port)
		}
		return nil
	}
	number, err := strconv.ParseFloat(string(encoded), 64)
	if err != nil {
		return nil
	}
	port := int(number)
	if float64(port) == number {
		*value = vmessIdentityPort(port)
	}
	return nil
}

type vmessIdentityPayload struct {
	Address vmessIdentityString `json:"add"`
	ID      vmessIdentityString `json:"id"`
	Port    vmessIdentityPort   `json:"port"`
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
		if b, err := base64x.DecodeString(b64Str); err == nil {
			var d vmessIdentityPayload
			if err := json.Unmarshal(b, &d); err == nil {
				return "vmess", string(d.ID), string(d.Address), int(d.Port), true
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
			b, err := base64x.DecodeString(body[:idx])
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
	if scheme, userinfo, host, port, ok := parseSimpleNodeIdentity(rawURI); ok {
		return scheme, userinfo, host, port, true
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

// parseSimpleNodeIdentity handles the overwhelmingly common unescaped
// scheme://userinfo@host:port form without allocating. Complex authorities
// deliberately fall back to net/url in parseNodeIdentity.
func parseSimpleNodeIdentity(rawURI string) (scheme, userinfo, host string, port int, ok bool) {
	separator := strings.Index(rawURI, "://")
	if separator <= 0 || !validProxyScheme(rawURI[:separator]) {
		return "", "", "", 0, false
	}
	authority := rawURI[separator+3:]
	if end := strings.IndexAny(authority, "/?#"); end >= 0 {
		authority = authority[:end]
	}
	if authority == "" || strings.ContainsAny(authority, "%\\") {
		return "", "", "", 0, false
	}

	hostPort := authority
	if at := strings.LastIndexByte(authority, '@'); at >= 0 {
		rawUserinfo := authority[:at]
		if colon := strings.IndexByte(rawUserinfo, ':'); colon >= 0 {
			rawUserinfo = rawUserinfo[:colon]
		}
		userinfo = rawUserinfo
		hostPort = authority[at+1:]
	}
	if hostPort == "" {
		return "", "", "", 0, false
	}

	port = 443
	if hostPort[0] == '[' {
		closeBracket := strings.IndexByte(hostPort, ']')
		if closeBracket <= 1 {
			return "", "", "", 0, false
		}
		host = hostPort[1:closeBracket]
		remainder := hostPort[closeBracket+1:]
		if remainder != "" {
			if remainder[0] != ':' {
				return "", "", "", 0, false
			}
			parsedPort, parsed := parseSimpleNodePort(remainder[1:])
			if !parsed {
				return "", "", "", 0, false
			}
			port = parsedPort
		}
	} else {
		if strings.Count(hostPort, ":") > 1 {
			return "", "", "", 0, false
		}
		host = hostPort
		if colon := strings.LastIndexByte(hostPort, ':'); colon >= 0 {
			host = hostPort[:colon]
			parsedPort, parsed := parseSimpleNodePort(hostPort[colon+1:])
			if !parsed {
				return "", "", "", 0, false
			}
			port = parsedPort
		}
	}
	if host == "" {
		return "", "", "", 0, false
	}
	return rawURI[:separator], userinfo, host, port, true
}

func parseSimpleNodePort(rawPort string) (int, bool) {
	if rawPort == "" {
		return 443, true
	}
	for index := range len(rawPort) {
		if rawPort[index] < '0' || rawPort[index] > '9' {
			return 0, false
		}
	}
	port, err := strconv.Atoi(rawPort)
	if err != nil {
		return 0, false
	}
	if port == 0 {
		port = 443
	}
	return port, true
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
		storeNodeHealthUnsafe(uri, h)
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
		storeNodeHealthUnsafe(uri, h)
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
		storeNodeHealthUnsafe(uri, health)
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
// 节点，同一类别内再按分数选择；只有候选实际进入堆时才复制 Node。
func retainHighestKnown(
	nodes []scoredNode,
	node *Node,
	score float64,
	recovering bool,
	limit int,
) []scoredNode {
	if limit <= 0 {
		return nodes
	}
	if len(nodes) < limit {
		nodes = append(nodes, scoredNode{node: *node, score: score, recovering: recovering})
		if len(nodes) == limit {
			for index := len(nodes)/2 - 1; index >= 0; index-- {
				siftDownKnownMinHeap(nodes, index)
			}
		}
		return nodes
	}
	// Under the healthy-first ordering, a healthy heap root proves every
	// retained node is healthy. Keep that common steady state on the simpler
	// score-only heap path instead of rechecking recovery state at each level.
	if !nodes[0].recovering && !recovering {
		if !(score > nodes[0].score) {
			return nodes
		}
		nodes[0] = scoredNode{node: *node, score: score}
		siftDownKnownHealthyMinHeap(nodes, 0)
		return nodes
	}
	if recovering != nodes[0].recovering {
		if recovering {
			return nodes
		}
	} else if !(score > nodes[0].score) {
		return nodes
	}
	nodes[0] = scoredNode{node: *node, score: score, recovering: recovering}
	siftDownKnownMinHeap(nodes, 0)
	return nodes
}

func knownNodeBetter(left, right scoredNode) bool {
	if left.recovering != right.recovering {
		return !left.recovering
	}
	return left.score > right.score
}

func siftDownKnownHealthyMinHeap(nodes []scoredNode, index int) {
	for {
		left := index*2 + 1
		if left >= len(nodes) {
			return
		}
		worst := left
		if right := left + 1; right < len(nodes) && nodes[left].score > nodes[right].score {
			worst = right
		}
		if !(nodes[index].score > nodes[worst].score) {
			return
		}
		nodes[index], nodes[worst] = nodes[worst], nodes[index]
		index = worst
	}
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

type healthCheckCandidate struct {
	priority int
	lastSeen int64
	order    int
}

func healthCheckEarlier(left, right healthCheckCandidate) bool {
	if left.priority != right.priority {
		return left.priority < right.priority
	}
	if left.lastSeen != right.lastSeen {
		return left.lastSeen < right.lastSeen
	}
	return left.order < right.order
}

func retainEarliestHealthCheck(
	candidates []healthCheckCandidate,
	candidate healthCheckCandidate,
	limit int,
) []healthCheckCandidate {
	if len(candidates) < limit {
		candidates = append(candidates, candidate)
		if len(candidates) == limit {
			for index := len(candidates)/2 - 1; index >= 0; index-- {
				siftDownHealthCheckMaxHeap(candidates, index)
			}
		}
		return candidates
	}
	if !healthCheckEarlier(candidate, candidates[0]) {
		return candidates
	}
	candidates[0] = candidate
	siftDownHealthCheckMaxHeap(candidates, 0)
	return candidates
}

func siftDownHealthCheckMaxHeap(candidates []healthCheckCandidate, index int) {
	for {
		left := index*2 + 1
		if left >= len(candidates) {
			return
		}
		latest := left
		if right := left + 1; right < len(candidates) &&
			healthCheckEarlier(candidates[left], candidates[right]) {
			latest = right
		}
		if !healthCheckEarlier(candidates[index], candidates[latest]) {
			return
		}
		candidates[index], candidates[latest] = candidates[latest], candidates[index]
		index = latest
	}
}

func GetNodePoolStats(now time.Time) NodePoolStats {
	lockLoadedForRead()
	defer mu.RUnlock()
	stats := NodePoolStats{Total: len(nodeList)}
	nowUnix := now.Unix()
	indexedHealth := len(healthByNodeIndex) == len(nodeList)
	for index, node := range nodeList {
		var health *NodeHealth
		if indexedHealth {
			health = healthByNodeIndex[index]
		} else {
			health = healthMap[node.RawURI]
		}
		addNodePoolStats(&stats, node, health, nowUnix)
	}
	return stats
}

func addNodePoolStats(stats *NodePoolStats, node Node, health *NodeHealth, nowUnix int64) {
	if node.Disabled {
		stats.Disabled++
		return
	}
	stats.Enabled++
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

// SelectNodesForHealthCheck 按“未测试、冷却到期失败、健康记录过期”的顺序选择巡检节点。
func SelectNodesForHealthCheck(limit int, staleAfter time.Duration, now time.Time) []Node {
	if limit <= 0 {
		return nil
	}
	lockLoadedForRead()

	nowUnix := now.Unix()
	staleBefore := now.Add(-staleAfter).Unix()
	candidateLimit := min(limit, len(nodeList))
	const inlineHealthCheckCandidateLimit = 64
	var inlineCandidates [inlineHealthCheckCandidateLimit]healthCheckCandidate
	var candidates []healthCheckCandidate
	if candidateLimit <= len(inlineCandidates) {
		candidates = inlineCandidates[:0:candidateLimit]
	} else {
		candidates = make([]healthCheckCandidate, 0, candidateLimit)
	}
	indexedHealth := len(healthByNodeIndex) == len(nodeList)
	for order, node := range nodeList {
		if node.Disabled {
			continue
		}
		var health *NodeHealth
		if indexedHealth {
			health = healthByNodeIndex[order]
		} else {
			health = healthMap[node.RawURI]
		}
		var candidate healthCheckCandidate
		switch {
		case health == nil || (health.LastSuccessAt == 0 && health.LastFailAt == 0):
			candidate = healthCheckCandidate{priority: 0, order: order}
		case health.CooldownUntil > nowUnix:
			continue
		case health.ConsecutiveFailures > 0:
			candidate = healthCheckCandidate{
				priority: 1, lastSeen: health.LastFailAt, order: order,
			}
		case staleAfter <= 0 || health.LastSuccessAt <= staleBefore:
			candidate = healthCheckCandidate{
				priority: 2, lastSeen: health.LastSuccessAt, order: order,
			}
		default:
			continue
		}
		candidates = retainEarliestHealthCheck(candidates, candidate, limit)
	}
	slices.SortFunc(candidates, func(left, right healthCheckCandidate) int {
		if healthCheckEarlier(left, right) {
			return -1
		}
		if healthCheckEarlier(right, left) {
			return 1
		}
		return 0
	})
	selected := make([]Node, len(candidates))
	for i := range candidates {
		selected[i] = nodeList[candidates[i].order]
	}
	mu.RUnlock()
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
	// Sticky membership changes much less often than request selection. Resolve
	// sparse immutable URI snapshots to sorted node indexes once, then consume
	// them alongside the nodeList scan. Dense snapshots retain direct map
	// lookups to avoid sorting or allocating an index slice per request.
	const inlineStickyIndexLimit = 128
	var inlineStickyIndexes [inlineStickyIndexLimit]int
	var stickyIndexes []int
	indexedSticky := len(stickyNodes) > 0 && len(stickyNodes) <= len(inlineStickyIndexes)
	if indexedSticky {
		stickyIndexes = inlineStickyIndexes[:0:len(stickyNodes)]
		for uri := range stickyNodes {
			index, exists := nodeIndexByURI[uri]
			if !exists || index < 0 || index >= len(nodeList) || nodeList[index].RawURI != uri {
				// The read index can briefly be stale after an external list
				// mutation. Fall back to URI membership for exact semantics.
				indexedSticky = false
				stickyIndexes = nil
				break
			}
			stickyIndexes = append(stickyIndexes, index)
		}
		if indexedSticky {
			slices.Sort(stickyIndexes)
		}
	}
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
	stickyPosition := 0
	indexedHealth := len(healthByNodeIndex) == len(nodeList)
	for nodeIndex := range nodeList {
		n := &nodeList[nodeIndex]
		sticky := false
		if indexedSticky {
			sticky = stickyPosition < len(stickyIndexes) && stickyIndexes[stickyPosition] == nodeIndex
			if sticky {
				stickyPosition++
			}
		} else if len(stickyNodes) > 0 {
			_, sticky = stickyNodes[n.RawURI]
		}
		if n.Disabled {
			continue
		}
		var h *NodeHealth
		if indexedHealth {
			h = healthByNodeIndex[nodeIndex]
		} else {
			// Keep exact behavior while a derived index length is temporarily
			// stale inside an internal write path.
			h = healthMap[n.RawURI]
		}
		if h != nil && h.CooldownUntil > now {
			if cooldownNodes == nil {
				if auxiliaryLimit <= len(inlineCooldown) {
					cooldownNodes = inlineCooldown[:0:auxiliaryLimit]
				} else {
					cooldownNodes = make([]scoredNode, 0, auxiliaryLimit)
				}
			}
			cooldownNodes = retainEarliestCooldown(cooldownNodes, scoredNode{
				node: *n, score: float64(h.CooldownUntil), last429: h.Last429At,
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
			item := scoredNode{node: *n, score: 80}
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
			score += selectionUpperBound(float64(h.SuccessCount), 100) * 3
			score -= selectionUpperBound(float64(h.FailCount), 100) * 4
			score -= float64(h.ConsecutiveFailures) * 25
			if h.LastTestMs > 0 {
				score -= selectionUpperBound(h.LastTestMs/1000.0, 30)
			}
			lastSeen := maxInt64(h.LastSuccessAt, h.LastFailAt)
			if now-lastSeen > 3600 {
				score += 10
			}
			score -= selectionUpperBound(float64(h.RateLimitCount), 10) * 5
			score -= float64(h.RecentUseCount) * 2
			if h.LastSelectedAt > 0 {
				elapsed := now - h.LastSelectedAt
				if elapsed > 600 {
					score += selectionUpperBound(float64(elapsed)/60, 15)
				}
			}
		}
		if sticky {
			score += 40
		}
		if known == nil {
			if knownLimit <= len(inlineKnown) {
				known = inlineKnown[:0:knownLimit]
			} else {
				known = make([]scoredNode, 0, knownLimit)
			}
		}
		if score < 1 {
			score = 1
		}
		recovering := h.ConsecutiveFailures > 0
		if len(known) == knownLimit && !known[0].recovering && !recovering {
			// Once a full heap has a healthy root, every retained node is
			// healthy. Keep the overwhelmingly common rejection path local so
			// it avoids the non-inlineable mixed-state heap helper.
			if score > known[0].score {
				known[0] = scoredNode{node: *n, score: score}
				siftDownKnownHealthyMinHeap(known, 0)
			}
		} else {
			known = retainHighestKnown(known, n, score, recovering, knownLimit)
		}
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

func selectionUpperBound(value, upper float64) float64 {
	if value > upper {
		return upper
	}
	return value
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
	var pool []weightedCandidate
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
	indexedHealth := len(healthByNodeIndex) == len(nodeList)
	for index, n := range nodeList {
		if n.Disabled {
			continue
		}
		var h *NodeHealth
		if indexedHealth {
			h = healthByNodeIndex[index]
		} else {
			h = healthMap[n.RawURI]
		}
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
