package audit

import (
	"context"
	"fmt"
)

// VerifyResult reports whether the chain is intact and, if not, the
// first row where it breaks -- everything after that point is
// untrustworthy regardless of whether later rows individually
// "verify," since a break means the chain was forked or rows were
// altered/removed at that point.
type VerifyResult struct {
	OK          bool
	FirstBadID  int64 // 0 if OK
	RowsChecked int64
}

// VerifyChain walks audit_log in id order, recomputing each row's hash
// from its own fields plus the previous row's hash, and confirms it
// matches the stored row_hash and that prev_hash matches the actual
// previous row -- catching both in-place tampering (a row's fields
// changed, its stored row_hash no longer matches what recomputing it
// produces) and forgery (a row inserted with a prev_hash that doesn't
// match what actually preceded it).
//
// This proves internal consistency only. It cannot detect an attacker
// who deletes the whole table and replays a self-consistent chain from
// row 1 -- that's what checkpoint.go's external anchoring is for. Run
// both in the runbook/threat-model verification, not just this one.
func (s *Store) VerifyChain(ctx context.Context) (VerifyResult, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, tenant_id, user_id, source, event_type, query_text, row_count,
		       duration_ms, status, error_message, detail, prev_hash, row_hash
		FROM audit_log ORDER BY id ASC`)
	if err != nil {
		return VerifyResult{}, fmt.Errorf("audit: querying for verification: %w", err)
	}
	defer rows.Close()

	var expectedPrevHash *string
	var checked int64
	for rows.Next() {
		var rec Record
		if err := rows.Scan(&rec.ID, &rec.TenantID, &rec.UserID, &rec.Source, &rec.EventType,
			&rec.QueryText, &rec.RowCount, &rec.DurationMS, &rec.Status, &rec.ErrorMessage,
			&rec.Detail, &rec.PrevHash, &rec.RowHash); err != nil {
			return VerifyResult{}, fmt.Errorf("audit: scanning row for verification: %w", err)
		}
		checked++

		if !hashPtrEqual(rec.PrevHash, expectedPrevHash) {
			return VerifyResult{OK: false, FirstBadID: rec.ID, RowsChecked: checked}, nil
		}
		recomputed := computeHash(rec.PrevHash, rec.Entry)
		if recomputed != rec.RowHash {
			return VerifyResult{OK: false, FirstBadID: rec.ID, RowsChecked: checked}, nil
		}
		hash := rec.RowHash
		expectedPrevHash = &hash
	}
	if err := rows.Err(); err != nil {
		return VerifyResult{}, fmt.Errorf("audit: reading verification rows: %w", err)
	}

	return VerifyResult{OK: true, RowsChecked: checked}, nil
}

func hashPtrEqual(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// ListForTenant reads a tenant's audit trail, most recent first --
// what a tenant Admin/Owner sees per /docs/phase-4-rbac-design.md's
// permission matrix.
func (s *Store) ListForTenant(ctx context.Context, tenantID string, limit int) ([]Record, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, tenant_id, user_id, source, event_type, query_text, row_count,
		       duration_ms, status, error_message, detail, prev_hash, row_hash
		FROM audit_log WHERE tenant_id = $1 ORDER BY id DESC LIMIT $2`, tenantID, limit)
	if err != nil {
		return nil, fmt.Errorf("audit: listing for tenant: %w", err)
	}
	defer rows.Close()

	var out []Record
	for rows.Next() {
		var rec Record
		if err := rows.Scan(&rec.ID, &rec.TenantID, &rec.UserID, &rec.Source, &rec.EventType,
			&rec.QueryText, &rec.RowCount, &rec.DurationMS, &rec.Status, &rec.ErrorMessage,
			&rec.Detail, &rec.PrevHash, &rec.RowHash); err != nil {
			return nil, fmt.Errorf("audit: scanning row: %w", err)
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}
