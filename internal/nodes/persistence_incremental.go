package nodes

import (
	"database/sql"
	"fmt"

	"github.com/bsfdsagfadg/vertex/internal/db"
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
		stmt, err := tx.Prepare("UPDATE nodes SET source_id = 0 WHERE raw_uri = ?")
		if err != nil {
			return rollback(err)
		}
		for _, change := range manualized {
			if _, err := stmt.Exec(change.rawURI); err != nil {
				_ = stmt.Close()
				return rollback(err)
			}
		}
		if err := stmt.Close(); err != nil {
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

// persistNodeSnapshotDiffUnsafe reconciles the current in-memory nodes against
// a caller-provided node snapshot. previousMemberships may be nil when the
// caller guarantees that subscription relationships did not change.
func persistNodeSnapshotDiffUnsafe(
	previousNodes []Node,
	previousMemberships map[membershipKey]bool,
) error {
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

	membershipsAdded := make([]membershipKey, 0)
	membershipsRemoved := make([]membershipKey, 0)
	affectedSourceIDs := make(map[int64]bool)
	if previousMemberships != nil {
		currentMemberships := flattenMemberships(subscriptionSources)
		for membership := range currentMemberships {
			if !previousMemberships[membership] {
				membershipsAdded = append(membershipsAdded, membership)
				affectedSourceIDs[membership.sourceID] = true
			}
		}
		for membership := range previousMemberships {
			if !currentMemberships[membership] {
				membershipsRemoved = append(membershipsRemoved, membership)
				affectedSourceIDs[membership.sourceID] = true
			}
		}
	}
	positionChanges := changedNodePositionsUnsafe(previousNodes)
	if len(inserted) == 0 && len(updated) == 0 && len(removedNodes) == 0 &&
		len(membershipsAdded) == 0 && len(membershipsRemoved) == 0 &&
		len(positionChanges) == 0 {
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

	if err := insertNodesTx(tx, inserted); err != nil {
		return rollback(err)
	}
	if err := updateNodesTx(tx, updated); err != nil {
		return rollback(err)
	}
	if err := applyMembershipChangesTx(tx, membershipsAdded, membershipsRemoved); err != nil {
		return rollback(err)
	}
	if err := deleteNodesTx(tx, removedNodes); err != nil {
		return rollback(err)
	}
	if err := updateNodePositionsTx(tx, positionChanges); err != nil {
		return rollback(err)
	}
	if err := updateSubscriptionCountsTx(tx, affectedSourceIDs); err != nil {
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

func flattenMemberships(sources map[string]map[int64]bool) map[membershipKey]bool {
	out := make(map[membershipKey]bool)
	for rawURI, sourceIDs := range sources {
		for sourceID, present := range sourceIDs {
			if present {
				out[membershipKey{sourceID: sourceID, rawURI: rawURI}] = true
			}
		}
	}
	return out
}

func insertNodesTx(tx *sql.Tx, inserted []nodeInsert) error {
	if len(inserted) == 0 {
		return nil
	}
	stmt, err := tx.Prepare(`INSERT INTO nodes
		(raw_uri, type, name, disabled, source_id, sort_order)
		VALUES (?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	for _, item := range inserted {
		node := item.node
		if _, err := stmt.Exec(
			node.RawURI,
			node.Type,
			node.Name,
			node.Disabled,
			node.SourceID,
			item.sortOrder,
		); err != nil {
			_ = stmt.Close()
			return err
		}
	}
	return stmt.Close()
}

func updateNodesTx(tx *sql.Tx, updated []Node) error {
	if len(updated) == 0 {
		return nil
	}
	stmt, err := tx.Prepare(`UPDATE nodes
		SET type = ?, name = ?, disabled = ?, source_id = ?
		WHERE raw_uri = ?`)
	if err != nil {
		return err
	}
	for _, node := range updated {
		if _, err := stmt.Exec(
			node.Type,
			node.Name,
			node.Disabled,
			node.SourceID,
			node.RawURI,
		); err != nil {
			_ = stmt.Close()
			return err
		}
	}
	return stmt.Close()
}

func applyMembershipChangesTx(
	tx *sql.Tx,
	added []membershipKey,
	removed []membershipKey,
) error {
	if len(added) > 0 {
		stmt, err := tx.Prepare(`INSERT INTO proxy_subscription_nodes(subscription_id, raw_uri)
			VALUES (?, ?)`)
		if err != nil {
			return err
		}
		for _, membership := range added {
			if _, err := stmt.Exec(membership.sourceID, membership.rawURI); err != nil {
				_ = stmt.Close()
				return err
			}
		}
		if err := stmt.Close(); err != nil {
			return err
		}
	}
	if len(removed) == 0 {
		return nil
	}
	stmt, err := tx.Prepare(
		"DELETE FROM proxy_subscription_nodes WHERE subscription_id = ? AND raw_uri = ?",
	)
	if err != nil {
		return err
	}
	for _, membership := range removed {
		if _, err := stmt.Exec(membership.sourceID, membership.rawURI); err != nil {
			_ = stmt.Close()
			return err
		}
	}
	return stmt.Close()
}

func updateSubscriptionCountsTx(tx *sql.Tx, sourceIDs map[int64]bool) error {
	if len(sourceIDs) == 0 {
		return nil
	}
	stmt, err := tx.Prepare(`UPDATE proxy_subscriptions
		SET node_count = (
			SELECT COUNT(*) FROM proxy_subscription_nodes
			WHERE subscription_id = ?
		)
		WHERE id = ?`)
	if err != nil {
		return err
	}
	for sourceID := range sourceIDs {
		if _, err := stmt.Exec(sourceID, sourceID); err != nil {
			_ = stmt.Close()
			return err
		}
	}
	return stmt.Close()
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
	if len(changed) == 0 {
		return nil
	}
	stmt, err := tx.Prepare("UPDATE nodes SET sort_order = ? WHERE raw_uri = ?")
	if err != nil {
		return err
	}
	for _, item := range changed {
		if _, err := stmt.Exec(item.sortOrder, item.node.RawURI); err != nil {
			_ = stmt.Close()
			return err
		}
	}
	return stmt.Close()
}

func updateSubscriptionMembershipsTx(
	tx *sql.Tx,
	sourceID int64,
	added []string,
	removed []string,
) error {
	if len(added) > 0 {
		stmt, err := tx.Prepare(`INSERT INTO proxy_subscription_nodes(subscription_id, raw_uri)
			VALUES (?, ?)`)
		if err != nil {
			return err
		}
		for _, rawURI := range added {
			if _, err := stmt.Exec(sourceID, rawURI); err != nil {
				_ = stmt.Close()
				return err
			}
		}
		if err := stmt.Close(); err != nil {
			return err
		}
	}
	if len(removed) == 0 {
		return nil
	}
	stmt, err := tx.Prepare(
		"DELETE FROM proxy_subscription_nodes WHERE subscription_id = ? AND raw_uri = ?",
	)
	if err != nil {
		return err
	}
	for _, rawURI := range removed {
		if _, err := stmt.Exec(sourceID, rawURI); err != nil {
			_ = stmt.Close()
			return err
		}
	}
	return stmt.Close()
}

func deleteNodesTx(tx *sql.Tx, rawURIs []string) error {
	if len(rawURIs) == 0 {
		return nil
	}
	stmt, err := tx.Prepare("DELETE FROM nodes WHERE raw_uri = ?")
	if err != nil {
		return err
	}
	for _, rawURI := range rawURIs {
		if _, err := stmt.Exec(rawURI); err != nil {
			_ = stmt.Close()
			return err
		}
	}
	return stmt.Close()
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
