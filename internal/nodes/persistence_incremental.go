package nodes

import (
	"database/sql"
	"fmt"
	"slices"
	"strings"

	"github.com/bsfdsagfadg/vertex/internal/db"
)

const (
	nodeMutationBatchSize = 500
	// Multi-row writes stay small enough to avoid excessive SQLite parse cost.
	nodeWriteBatchSize       = 32
	membershipWriteBatchSize = 32
	// Smaller batches avoid SQLite's growing parse cost for large VALUES CTEs
	// while still replacing many per-row statements with one update.
	nodePositionBatchSize = 64
)

type nodeInsert struct {
	node      Node
	sortOrder int
}

type nodeManualization struct {
	rawURI           string
	index            int
	previousSourceID int64
}

type membershipKey struct {
	sourceID int64
	rawURI   string
}

type subscriptionSyncChanges struct {
	sourceID           int64
	count              int
	inserted           []nodeInsert
	updated            []Node
	membershipsAdded   []string
	membershipsRemoved []string
	removedNodes       []string
	positionChanges    []nodeInsert
}

type nodeSnapshotChanges struct {
	inserted           []nodeInsert
	updated            []Node
	membershipsAdded   []membershipKey
	membershipsRemoved []membershipKey
	removedNodes       []string
	positionChanges    []nodeInsert
	affectedSourceIDs  map[int64]bool
}

func updateNodesDisabledTx(tx *sql.Tx, rawURIs []string, disabled bool) error {
	for start := 0; start < len(rawURIs); start += nodeMutationBatchSize {
		end := min(start+nodeMutationBatchSize, len(rawURIs))
		batch := rawURIs[start:end]
		placeholders := sqlPlaceholders(len(batch))
		query := "UPDATE nodes SET disabled = ? WHERE raw_uri IN (" + placeholders + ")"
		args := make([]any, len(batch)+1)
		args[0] = disabled
		for index, rawURI := range batch {
			args[index+1] = rawURI
		}
		result, err := tx.Exec(query, args...)
		if err != nil {
			return err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("读取批量更新节点数量: %w", err)
		}
		if affected != int64(len(batch)) {
			return fmt.Errorf("批量更新节点数量不一致: updated %d, expected %d", affected, len(batch))
		}
	}
	return nil
}

func sqlPlaceholders(count int) string {
	return strings.TrimSuffix(strings.Repeat("?,", count), ",")
}

// persistMergedNodesUnsafe persists only the changes produced by MergeNodes.
// The caller holds mu and restores its in-memory changes if this returns an
// error.
func persistMergedNodesUnsafe(inserted []nodeInsert, manualized []nodeManualization) error {
	database := db.CurrentDB()
	if database == nil || len(inserted) == 0 && len(manualized) == 0 {
		return nil
	}

	tx, err := database.Begin()
	if err != nil {
		return fmt.Errorf("开始合并节点事务: %w", err)
	}
	rollback := func(mergeErr error) error {
		_ = tx.Rollback()
		return fmt.Errorf("合并节点事务: %w", mergeErr)
	}

	if len(manualized) > 0 {
		if err := manualizeNodesTx(tx, manualized); err != nil {
			return rollback(err)
		}
	}

	if err := insertNodesTx(tx, inserted); err != nil {
		return rollback(err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交合并节点事务: %w", err)
	}
	return nil
}

func manualizeNodesTx(tx *sql.Tx, manualized []nodeManualization) error {
	for start := 0; start < len(manualized); start += nodeMutationBatchSize {
		end := min(start+nodeMutationBatchSize, len(manualized))
		batch := manualized[start:end]
		query := "UPDATE nodes SET source_id = 0 WHERE raw_uri IN (" +
			sqlPlaceholders(len(batch)) + ")"
		args := make([]any, len(batch))
		for index, change := range batch {
			args[index] = change.rawURI
		}
		result, err := tx.Exec(query, args...)
		if err != nil {
			return err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("读取批量手动化节点数量: %w", err)
		}
		if affected != int64(len(batch)) {
			return fmt.Errorf(
				"批量手动化节点数量不一致: updated %d, expected %d",
				affected,
				len(batch),
			)
		}
	}
	return nil
}

// persistNodeOrderUnsafe updates only nodes whose position changed. The caller
// holds mu, has already sorted nodeList and has not yet published the new
// nodeIndexByURI positions.
func persistNodeOrderUnsafe() error {
	changed := make([]nodeInsert, 0)
	for index, node := range nodeList {
		previousIndex, exists := nodeIndexByURI[node.RawURI]
		if !exists {
			return fmt.Errorf("保存节点排序: 缺少节点索引 %q", node.RawURI)
		}
		if previousIndex != index {
			changed = append(changed, nodeInsert{node: node, sortOrder: index})
		}
	}
	if len(changed) == 0 {
		return nil
	}

	database := db.CurrentDB()
	if database == nil {
		return nil
	}
	tx, err := database.Begin()
	if err != nil {
		return fmt.Errorf("开始保存节点排序事务: %w", err)
	}
	rollback := func(sortErr error) error {
		_ = tx.Rollback()
		return fmt.Errorf("保存节点排序事务: %w", sortErr)
	}
	if err := updateNodePositionsTx(tx, changed); err != nil {
		return rollback(err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交节点排序事务: %w", err)
	}
	return nil
}

func persistSubscriptionSyncUnsafe(
	changes subscriptionSyncChanges,
	finalize func(*sql.Tx) error,
) error {
	database := db.CurrentDB()
	if database == nil {
		return fmt.Errorf("同步订阅节点: database unavailable")
	}
	tx, err := database.Begin()
	if err != nil {
		return fmt.Errorf("开始同步订阅节点事务: %w", err)
	}
	rollback := func(syncErr error) error {
		_ = tx.Rollback()
		return fmt.Errorf("同步订阅节点事务: %w", syncErr)
	}

	if err := insertNodesTx(tx, changes.inserted); err != nil {
		return rollback(err)
	}

	if err := updateNodesTx(tx, changes.updated); err != nil {
		return rollback(err)
	}

	if err := updateSubscriptionMembershipsTx(
		tx,
		changes.sourceID,
		changes.membershipsAdded,
		changes.membershipsRemoved,
	); err != nil {
		return rollback(err)
	}
	if err := deleteNodesTx(tx, changes.removedNodes); err != nil {
		return rollback(err)
	}
	if err := updateNodePositionsTx(tx, changes.positionChanges); err != nil {
		return rollback(err)
	}

	if finalize != nil {
		if err := finalize(tx); err != nil {
			return rollback(err)
		}
	} else {
		updateResult, err := tx.Exec(`UPDATE proxy_subscriptions SET node_count = ? WHERE id = ?`,
			changes.count, changes.sourceID)
		if err != nil {
			return rollback(err)
		}
		if affected, _ := updateResult.RowsAffected(); affected != 1 {
			return rollback(fmt.Errorf("proxy subscription not found"))
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交同步订阅节点事务: %w", err)
	}
	return nil
}

// persistNodeSnapshotDiffUnsafe reconciles current nodes against a snapshot
// when the indexed replacement fast path detects an unexpected stale index.
// Subscription relationships do not change in that replacement path.
func persistNodeSnapshotDiffUnsafe(previousNodes []Node) error {
	previousByURI := make(map[string]Node, len(previousNodes))
	for _, node := range previousNodes {
		previousByURI[node.RawURI] = node
	}
	currentByURI := make(map[string]Node, len(nodeList))
	inserted := make([]nodeInsert, 0)
	updated := make([]Node, 0)
	for index, node := range nodeList {
		currentByURI[node.RawURI] = node
		previous, existed := previousByURI[node.RawURI]
		if !existed {
			inserted = append(inserted, nodeInsert{node: node, sortOrder: index})
			continue
		}
		if !persistedNodeFieldsEqual(previous, node) {
			updated = append(updated, node)
		}
	}
	removedNodes := make([]string, 0)
	for rawURI := range previousByURI {
		if _, exists := currentByURI[rawURI]; !exists {
			removedNodes = append(removedNodes, rawURI)
		}
	}

	positionChanges := changedNodePositionsUnsafe(previousNodes)
	return persistNodeSnapshotChangesUnsafe(nodeSnapshotChanges{
		inserted:        inserted,
		updated:         updated,
		removedNodes:    removedNodes,
		positionChanges: positionChanges,
	})
}

// persistIndexedNodeReplacementUnsafe compares the replacement list against
// the still-published index of previousNodes. ReplaceManualNodes builds its new
// list in separate storage and does not rebuild nodeIndexByURI until after the
// transaction, so the old index replaces three full-pool temporary maps.
func persistIndexedNodeReplacementUnsafe(previousNodes []Node, removedNodes []string) error {
	changes := nodeSnapshotChanges{removedNodes: removedNodes}
	for index, node := range nodeList {
		previousIndex, existed := nodeIndexByURI[node.RawURI]
		if !existed {
			changes.inserted = append(changes.inserted, nodeInsert{node: node, sortOrder: index})
			continue
		}
		if previousIndex < 0 || previousIndex >= len(previousNodes) ||
			previousNodes[previousIndex].RawURI != node.RawURI {
			// Preserve correctness if an unexpected stale index reaches this path.
			return persistNodeSnapshotDiffUnsafe(previousNodes)
		}
		if !persistedNodeFieldsEqual(previousNodes[previousIndex], node) {
			changes.updated = append(changes.updated, node)
		}
		if previousIndex != index {
			changes.positionChanges = append(
				changes.positionChanges,
				nodeInsert{node: node, sortOrder: index},
			)
		}
	}
	return persistNodeSnapshotChangesUnsafe(changes)
}

func persistNodeSnapshotChangesUnsafe(changes nodeSnapshotChanges) error {
	if len(changes.inserted) == 0 && len(changes.updated) == 0 && len(changes.removedNodes) == 0 &&
		len(changes.membershipsAdded) == 0 && len(changes.membershipsRemoved) == 0 &&
		len(changes.positionChanges) == 0 {
		return nil
	}

	database := db.CurrentDB()
	if database == nil {
		return nil
	}
	tx, err := database.Begin()
	if err != nil {
		return fmt.Errorf("开始保存节点差异事务: %w", err)
	}
	rollback := func(diffErr error) error {
		_ = tx.Rollback()
		return fmt.Errorf("保存节点差异事务: %w", diffErr)
	}

	if err := insertNodesTx(tx, changes.inserted); err != nil {
		return rollback(err)
	}
	if err := updateNodesTx(tx, changes.updated); err != nil {
		return rollback(err)
	}
	if err := applyMembershipChangesTx(tx, changes.membershipsAdded, changes.membershipsRemoved); err != nil {
		return rollback(err)
	}
	if err := deleteNodesTx(tx, changes.removedNodes); err != nil {
		return rollback(err)
	}
	if err := updateNodePositionsTx(tx, changes.positionChanges); err != nil {
		return rollback(err)
	}
	if err := updateSubscriptionCountsTx(tx, changes.affectedSourceIDs); err != nil {
		return rollback(err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交节点差异事务: %w", err)
	}
	return nil
}

func persistedNodeFieldsEqual(left, right Node) bool {
	return left.RawURI == right.RawURI &&
		left.Type == right.Type &&
		left.Name == right.Name &&
		left.Disabled == right.Disabled &&
		left.SourceID == right.SourceID
}

func insertNodesTx(tx *sql.Tx, inserted []nodeInsert) error {
	for start := 0; start < len(inserted); start += nodeWriteBatchSize {
		end := min(start+nodeWriteBatchSize, len(inserted))
		batch := inserted[start:end]
		var query strings.Builder
		query.Grow(96 + len(batch)*14)
		query.WriteString(`INSERT INTO nodes
			(raw_uri, type, name, disabled, source_id, sort_order) VALUES `)
		args := make([]any, 0, len(batch)*6)
		for index, item := range batch {
			if index > 0 {
				query.WriteByte(',')
			}
			query.WriteString("(?,?,?,?,?,?)")
			node := item.node
			args = append(args,
				node.RawURI,
				node.Type,
				node.Name,
				node.Disabled,
				node.SourceID,
				item.sortOrder,
			)
		}
		result, err := tx.Exec(query.String(), args...)
		if err != nil {
			return err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("读取批量插入节点数量: %w", err)
		}
		if affected != int64(len(batch)) {
			return fmt.Errorf("批量插入节点数量不一致: inserted %d, expected %d", affected, len(batch))
		}
	}
	return nil
}

func updateNodesTx(tx *sql.Tx, updated []Node) error {
	for start := 0; start < len(updated); start += nodeWriteBatchSize {
		end := min(start+nodeWriteBatchSize, len(updated))
		batch := updated[start:end]
		var query strings.Builder
		query.Grow(220 + len(batch)*12)
		query.WriteString(
			"WITH updates(raw_uri, type, name, disabled, source_id) AS (VALUES ",
		)
		args := make([]any, 0, len(batch)*5)
		for index, node := range batch {
			if index > 0 {
				query.WriteByte(',')
			}
			query.WriteString("(?,?,?,?,?)")
			args = append(args,
				node.RawURI,
				node.Type,
				node.Name,
				node.Disabled,
				node.SourceID,
			)
		}
		query.WriteString(`)
			UPDATE nodes SET
				type = updates.type,
				name = updates.name,
				disabled = updates.disabled,
				source_id = updates.source_id
			FROM updates WHERE nodes.raw_uri = updates.raw_uri`)
		result, err := tx.Exec(query.String(), args...)
		if err != nil {
			return err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("读取批量更新节点数量: %w", err)
		}
		if affected != int64(len(batch)) {
			return fmt.Errorf("批量更新节点数量不一致: updated %d, expected %d", affected, len(batch))
		}
	}
	return nil
}

func applyMembershipChangesTx(
	tx *sql.Tx,
	added []membershipKey,
	removed []membershipKey,
) error {
	if err := insertMembershipsTx(tx, added); err != nil {
		return err
	}
	if len(removed) == 0 {
		return nil
	}
	removedBySource := make(map[int64][]string)
	for _, membership := range removed {
		removedBySource[membership.sourceID] = append(
			removedBySource[membership.sourceID],
			membership.rawURI,
		)
	}
	for sourceID, rawURIs := range removedBySource {
		if err := deleteSubscriptionMembershipsTx(tx, sourceID, rawURIs); err != nil {
			return err
		}
	}
	return nil
}

func insertMembershipsTx(tx *sql.Tx, memberships []membershipKey) error {
	for start := 0; start < len(memberships); start += membershipWriteBatchSize {
		end := min(start+membershipWriteBatchSize, len(memberships))
		batch := memberships[start:end]
		var query strings.Builder
		query.Grow(96 + len(batch)*6)
		query.WriteString(
			"INSERT INTO proxy_subscription_nodes(subscription_id, raw_uri) VALUES ",
		)
		args := make([]any, 0, len(batch)*2)
		for index, membership := range batch {
			if index > 0 {
				query.WriteByte(',')
			}
			query.WriteString("(?,?)")
			args = append(args, membership.sourceID, membership.rawURI)
		}
		result, err := tx.Exec(query.String(), args...)
		if err != nil {
			return err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("读取批量添加订阅关系数量: %w", err)
		}
		if affected != int64(len(batch)) {
			return fmt.Errorf(
				"批量添加订阅关系数量不一致: inserted %d, expected %d",
				affected,
				len(batch),
			)
		}
	}
	return nil
}

func deleteSubscriptionMembershipsTx(tx *sql.Tx, sourceID int64, rawURIs []string) error {
	for start := 0; start < len(rawURIs); start += nodeMutationBatchSize {
		end := min(start+nodeMutationBatchSize, len(rawURIs))
		batch := rawURIs[start:end]
		query := `DELETE FROM proxy_subscription_nodes
			WHERE subscription_id = ? AND raw_uri IN (` + sqlPlaceholders(len(batch)) + ")"
		args := make([]any, len(batch)+1)
		args[0] = sourceID
		for index, rawURI := range batch {
			args[index+1] = rawURI
		}
		result, err := tx.Exec(query, args...)
		if err != nil {
			return err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("读取批量删除订阅关系数量: %w", err)
		}
		if affected != int64(len(batch)) {
			return fmt.Errorf(
				"批量删除订阅关系数量不一致: deleted %d, expected %d",
				affected,
				len(batch),
			)
		}
	}
	return nil
}

func updateSubscriptionCountsTx(tx *sql.Tx, sourceIDs map[int64]bool) error {
	if len(sourceIDs) == 0 {
		return nil
	}
	orderedSourceIDs := make([]int64, 0, len(sourceIDs))
	for sourceID := range sourceIDs {
		orderedSourceIDs = append(orderedSourceIDs, sourceID)
	}
	slices.Sort(orderedSourceIDs)
	for start := 0; start < len(orderedSourceIDs); start += nodeMutationBatchSize {
		end := min(start+nodeMutationBatchSize, len(orderedSourceIDs))
		batch := orderedSourceIDs[start:end]
		query := `UPDATE proxy_subscriptions SET node_count = (
			SELECT COUNT(*) FROM proxy_subscription_nodes
			WHERE subscription_id = proxy_subscriptions.id
		) WHERE id IN (` + sqlPlaceholders(len(batch)) + ")"
		args := make([]any, len(batch))
		for index, sourceID := range batch {
			args[index] = sourceID
		}
		result, err := tx.Exec(query, args...)
		if err != nil {
			return err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("读取批量更新订阅节点计数数量: %w", err)
		}
		if affected != int64(len(batch)) {
			return fmt.Errorf(
				"批量更新订阅节点计数数量不一致: updated %d, expected %d",
				affected,
				len(batch),
			)
		}
	}
	return nil
}

func changedNodePositionsUnsafe(previous []Node) []nodeInsert {
	previousPositions := make(map[string]int, len(previous))
	for index, node := range previous {
		previousPositions[node.RawURI] = index
	}
	changed := make([]nodeInsert, 0)
	for index, node := range nodeList {
		if oldIndex, existed := previousPositions[node.RawURI]; existed && oldIndex != index {
			changed = append(changed, nodeInsert{node: node, sortOrder: index})
		}
	}
	return changed
}

func updateNodePositionsTx(tx *sql.Tx, changed []nodeInsert) error {
	for start := 0; start < len(changed); start += nodePositionBatchSize {
		end := min(start+nodePositionBatchSize, len(changed))
		batch := changed[start:end]
		var query strings.Builder
		query.Grow(160 + len(batch)*6)
		query.WriteString("WITH updates(raw_uri, sort_order) AS (VALUES ")
		args := make([]any, 0, len(batch)*2)
		for _, item := range batch {
			if len(args) > 0 {
				query.WriteByte(',')
			}
			query.WriteString("(?,?)")
			args = append(args, item.node.RawURI, item.sortOrder)
		}
		query.WriteString(`)
			UPDATE nodes SET sort_order = updates.sort_order
			FROM updates WHERE nodes.raw_uri = updates.raw_uri`)
		result, err := tx.Exec(query.String(), args...)
		if err != nil {
			return err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("读取节点排序更新数量: %w", err)
		}
		if affected != int64(len(batch)) {
			return fmt.Errorf("节点排序更新数量不一致: updated %d, expected %d", affected, len(batch))
		}
	}
	return nil
}

func updateSubscriptionMembershipsTx(
	tx *sql.Tx,
	sourceID int64,
	added []string,
	removed []string,
) error {
	if len(added) > 0 {
		memberships := make([]membershipKey, len(added))
		for index, rawURI := range added {
			memberships[index] = membershipKey{sourceID: sourceID, rawURI: rawURI}
		}
		if err := insertMembershipsTx(tx, memberships); err != nil {
			return err
		}
	}
	if len(removed) == 0 {
		return nil
	}
	return deleteSubscriptionMembershipsTx(tx, sourceID, removed)
}

func deleteNodesTx(tx *sql.Tx, rawURIs []string) error {
	for start := 0; start < len(rawURIs); start += nodeMutationBatchSize {
		end := min(start+nodeMutationBatchSize, len(rawURIs))
		batch := rawURIs[start:end]
		query := "DELETE FROM nodes WHERE raw_uri IN (" + sqlPlaceholders(len(batch)) + ")"
		args := make([]any, len(batch))
		for index, rawURI := range batch {
			args[index] = rawURI
		}
		if _, err := tx.Exec(query, args...); err != nil {
			return err
		}
	}
	return nil
}

func deletePersistedNodesUnsafe(rawURIs []string, affectedSourceIDs map[int64]bool) error {
	database := db.CurrentDB()
	if database == nil || len(rawURIs) == 0 {
		return nil
	}

	tx, err := database.Begin()
	if err != nil {
		return fmt.Errorf("开始删除节点事务: %w", err)
	}
	rollback := func(deleteErr error) error {
		_ = tx.Rollback()
		return fmt.Errorf("删除节点事务: %w", deleteErr)
	}

	seen := make(map[string]bool, len(rawURIs))
	uniqueRawURIs := make([]string, 0, len(rawURIs))
	for _, rawURI := range rawURIs {
		if seen[rawURI] {
			continue
		}
		seen[rawURI] = true
		uniqueRawURIs = append(uniqueRawURIs, rawURI)
	}
	if err := deleteNodesTx(tx, uniqueRawURIs); err != nil {
		return rollback(err)
	}

	if err := updateSubscriptionCountsTx(tx, affectedSourceIDs); err != nil {
		return rollback(err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交删除节点事务: %w", err)
	}
	return nil
}
