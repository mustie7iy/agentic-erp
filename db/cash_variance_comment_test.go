package db

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// commentRow is a variance comment on the fixture's snapshot sid: a
// person's on PAY from 17 to 23 April at lag 0, accepted, with the columns
// of set changed.
func commentRow(f fixture, sid uuid.UUID, set map[string]any) stmt {
	row := map[string]any{"legal_entity_id": f.entity, "period_from": "2026-04-17", "period_to": "2026-04-23", "lag": 0, "liquidity_item": "PAY",
		"text": "The run was released a week late.", "status": "accepted", "decided_by": f.actor, "decided_at": "2026-04-24T10:00:00Z"}
	for k, v := range set {
		row[k] = v
	}
	return step(`INSERT INTO cash.variance_comment (tenant_id, id, legal_entity_id, period_from, period_to, lag, liquidity_item, snapshot_id, text, status,
			decided_by, decided_at, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
		f.tenant, id(), row["legal_entity_id"], row["period_from"], row["period_to"], row["lag"], row["liquidity_item"], sid, row["text"], row["status"],
		row["decided_by"], row["decided_at"], f.actor)
}

// TestCashVarianceCommentRules: every rule the database enforces on
// variance comments (T53f4, 000201): a lag the variance offers, a period
// forwards, a text of 1 to 2,000 characters that is not blank, a status
// of the three, decided exactly when not proposed, an item of the tenant
// (none: the unclassified row), a snapshot of the comment's company;
// insert-only and never deleted.
func TestCashVarianceCommentRules(t *testing.T) {
	need(t)
	for _, c := range []struct {
		name string
		run  func(f fixture, sid uuid.UUID) []stmt
		want string // empty: allowed
	}{
		{"a person's comment", func(f fixture, sid uuid.UUID) []stmt { return []stmt{commentRow(f, sid, nil)} }, ""},
		{"on the unclassified row", func(f fixture, sid uuid.UUID) []stmt {
			return []stmt{commentRow(f, sid, map[string]any{"liquidity_item": nil})}
		}, ""},
		{"on an inactive item", func(f fixture, sid uuid.UUID) []stmt {
			return []stmt{commentRow(f, sid, map[string]any{"liquidity_item": "OLD"})}
		}, ""},
		{"an agent's proposal", func(f fixture, sid uuid.UUID) []stmt {
			return []stmt{commentRow(f, sid, map[string]any{"status": "proposed", "decided_by": nil, "decided_at": nil})}
		}, ""},
		{"a proposal rejected", func(f fixture, sid uuid.UUID) []stmt {
			return []stmt{commentRow(f, sid, map[string]any{"status": "rejected"})}
		}, ""},
		{"a lag not offered", func(f fixture, sid uuid.UUID) []stmt { return []stmt{commentRow(f, sid, map[string]any{"lag": 5})} },
			"variance_comment_lag_check"},
		{"a period backwards", func(f fixture, sid uuid.UUID) []stmt {
			return []stmt{commentRow(f, sid, map[string]any{"period_to": "2026-04-16"})}
		}, "variance_comment_period"},
		{"a period of a day", func(f fixture, sid uuid.UUID) []stmt {
			return []stmt{commentRow(f, sid, map[string]any{"period_to": "2026-04-17"})}
		}, ""},
		{"no text", func(f fixture, sid uuid.UUID) []stmt { return []stmt{commentRow(f, sid, map[string]any{"text": ""})} },
			"variance_comment_text"},
		{"a blank text", func(f fixture, sid uuid.UUID) []stmt {
			return []stmt{commentRow(f, sid, map[string]any{"text": " \n\t"})}
		},
			"variance_comment_text"},
		{"a text of 2,000 characters", func(f fixture, sid uuid.UUID) []stmt {
			return []stmt{commentRow(f, sid, map[string]any{"text": strings.Repeat("é", 2000)})}
		}, ""},
		{"a text of 2,001 characters", func(f fixture, sid uuid.UUID) []stmt {
			return []stmt{commentRow(f, sid, map[string]any{"text": strings.Repeat("é", 2001)})}
		}, "variance_comment_text"},
		{"a status", func(f fixture, sid uuid.UUID) []stmt {
			return []stmt{commentRow(f, sid, map[string]any{"status": "draft"})}
		},
			"variance_comment_status_check"},
		{"accepted by no one", func(f fixture, sid uuid.UUID) []stmt {
			return []stmt{commentRow(f, sid, map[string]any{"decided_by": nil, "decided_at": nil})}
		}, "variance_comment_decided"},
		{"a proposal decided", func(f fixture, sid uuid.UUID) []stmt {
			return []stmt{commentRow(f, sid, map[string]any{"status": "proposed"})}
		}, "variance_comment_decided"},
		{"decided at no time", func(f fixture, sid uuid.UUID) []stmt {
			return []stmt{commentRow(f, sid, map[string]any{"decided_at": nil})}
		}, "variance_comment_decided"},
		{"an item not the tenant's", func(f fixture, sid uuid.UUID) []stmt {
			return []stmt{commentRow(f, sid, map[string]any{"liquidity_item": "NOPE"})}
		}, "variance_comment_tenant_id_liquidity_item_fkey"},
		{"another company's snapshot", func(f fixture, sid uuid.UUID) []stmt {
			return []stmt{commentRow(f, sid, map[string]any{"legal_entity_id": id()})}
		}, "variance_comment_snapshot"},
		{"no snapshot", func(f fixture, _ uuid.UUID) []stmt { return []stmt{commentRow(f, id(), nil)} }, "variance_comment_snapshot"},
		{"a comment changed", func(f fixture, sid uuid.UUID) []stmt {
			return []stmt{commentRow(f, sid, nil), step(`UPDATE cash.variance_comment SET text = 'Other.' WHERE snapshot_id = $1`, sid)}
		}, "permission denied"},
		{"a comment deleted", func(f fixture, sid uuid.UUID) []stmt {
			return []stmt{commentRow(f, sid, nil), step(`DELETE FROM cash.variance_comment WHERE snapshot_id = $1`, sid)}
		}, "permission denied"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			sid := id()
			err := asApp(t, f.tenant, func(tx pgx.Tx) error {
				return all(tx, append([]stmt{memoItems(f), snapshotRow(f, sid, nil)}, c.run(f, sid)...)...)
			})
			if c.want != "" {
				failsWith(t, err, c.want)
				return
			}
			if err != nil {
				t.Fatalf("got %v, want no error", err)
			}
		})
	}
}

// TestCashVarianceCommentPrivileges: the comment permission is seeded and
// is no approval; the application may insert and read comments, never
// change or delete them (T53f4, 000201).
func TestCashVarianceCommentPrivileges(t *testing.T) {
	need(t)
	var approval bool
	if err := testDB.QueryRow(context.Background(), `SELECT is_approval FROM ref.permission WHERE code = 'cash.variance.comment'`).Scan(&approval); err != nil {
		t.Fatal(err)
	}
	if approval {
		t.Error("cash.variance.comment: got an approval permission, want none")
	}
	for _, c := range []struct {
		check string
		want  bool
	}{
		{`has_table_privilege('erp_app', 'cash.variance_comment', 'INSERT')`, true},
		{`has_table_privilege('erp_app', 'cash.variance_comment', 'UPDATE')`, false},
		{`has_table_privilege('erp_app', 'cash.variance_comment', 'DELETE')`, false},
		{`has_table_privilege('erp_worker', 'cash.variance_comment', 'SELECT')`, false},
		{`(SELECT relrowsecurity AND relforcerowsecurity FROM pg_class WHERE oid = 'cash.variance_comment'::regclass)`, true},
	} {
		t.Run(c.check, func(t *testing.T) {
			var got bool
			if err := testDB.QueryRow(context.Background(), `SELECT `+c.check).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

// TestCashSnapshotRetentionKeepsCommented: the retention function keeps a
// snapshot past its 25 months while a variance comment refers to it, and
// still deletes one of the same date that none refers to (T53f4, 000201;
// ADR-0051 §6).
func TestCashSnapshotRetentionKeepsCommented(t *testing.T) {
	need(t)
	f := newFixture(t)
	commented, plain := id(), id()
	var deleted []uuid.UUID
	var left []uuid.UUID
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		err := all(tx, memoItems(f),
			snapshotRow(f, commented, map[string]any{"snapshot_date": "2024-02-01", "horizon_end": "2024-12-31", "kind": "on_demand", "name": "Board"}),
			flowRow(f, commented, 1, nil),
			snapshotRow(f, plain, map[string]any{"snapshot_date": "2024-02-01", "horizon_end": "2024-12-31"}),
			commentRow(f, commented, map[string]any{"period_from": "2024-02-01", "period_to": "2024-02-07"}))
		if err != nil {
			return err
		}
		rows, _ := tx.Query(context.Background(), `SELECT deleted_id FROM cash.delete_expired_snapshots($1, '2026-04-17')`, f.entity)
		if deleted, err = pgx.CollectRows(rows, pgx.RowTo[uuid.UUID]); err != nil {
			return err
		}
		rows, _ = tx.Query(context.Background(), `SELECT id FROM cash.snapshot WHERE id IN ($1, $2)`, commented, plain)
		left, err = pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
		return err
	})
	if len(deleted) != 1 || deleted[0] != plain {
		t.Errorf("deleted: got %v, want only the snapshot no comment refers to, %v", deleted, plain)
	}
	if len(left) != 1 || left[0] != commented {
		t.Errorf("kept: got %v, want the commented snapshot %v", left, commented)
	}
}
