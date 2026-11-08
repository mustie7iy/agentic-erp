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
	return commentRowID(f, sid, id(), set)
}

// commentRowID is commentRow with the comment's id.
func commentRowID(f fixture, sid, cid uuid.UUID, set map[string]any) stmt {
	row := map[string]any{"legal_entity_id": f.entity, "period_from": "2026-04-17", "period_to": "2026-04-23", "lag": 0, "liquidity_item": "PAY",
		"text": "The run was released a week late.", "status": "accepted", "decided_by": f.actor, "decided_at": "2026-04-24T10:00:00Z",
		"session_id": nil, "citations": "[]", "decision_reason": nil, "version": 1, "proposed_text": nil}
	for k, v := range set {
		row[k] = v
	}
	return step(`INSERT INTO cash.variance_comment (tenant_id, id, legal_entity_id, period_from, period_to, lag, liquidity_item, snapshot_id, text, status,
			decided_by, decided_at, created_by, session_id, citations, decision_reason, version, proposed_text)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15::jsonb, $16, $17, $18)`,
		f.tenant, cid, row["legal_entity_id"], row["period_from"], row["period_to"], row["lag"], row["liquidity_item"], sid, row["text"], row["status"],
		row["decided_by"], row["decided_at"], f.actor, row["session_id"], row["citations"], row["decision_reason"], row["version"], row["proposed_text"])
}

// proposalOf are the columns of an agent's proposal, undecided, citing a
// payment.
func proposalOf() map[string]any {
	return map[string]any{"status": "proposed", "decided_by": nil, "decided_at": nil, "session_id": id(),
		"citations": `[{"kind": "payment", "id": "` + id().String() + `"}]`}
}

// TestCashVarianceCommentRules: every rule the database enforces on
// variance comments (T53f4, 000201): a lag the variance offers, a period
// forwards, a text of 1 to 2,000 characters that is not blank, a status
// of the three, decided exactly when not proposed, an item of the tenant
// (none: the unclassified row), a snapshot of the comment's company; and
// on the agent's proposals (T53i, 000218): a proposal names its session,
// only an agent's comment cites, a list of at most 50 citations, a reason
// exactly when rejected, a version from 1; a proposal decided once, by
// another than its author, its version raised by one, its text changed
// only by an accepted edit that keeps the agent's (proposed_text); what a
// comment explains never changed, and none deleted.
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
		{"an agent's proposal", func(f fixture, sid uuid.UUID) []stmt { return []stmt{commentRow(f, sid, proposalOf())} }, ""},
		{"a proposal rejected", func(f fixture, sid uuid.UUID) []stmt {
			return []stmt{commentRow(f, sid, with(proposalOf(), map[string]any{"status": "rejected", "decided_by": id(), "decided_at": "2026-04-24T10:00:00Z",
				"decision_reason": "Not so."}))}
		}, ""},
		{"a proposal accepted", func(f fixture, sid uuid.UUID) []stmt {
			return []stmt{commentRow(f, sid, with(proposalOf(), map[string]any{"status": "accepted", "decided_by": id(), "decided_at": "2026-04-24T10:00:00Z"}))}
		}, ""},
		{"a proposal without its session", func(f fixture, sid uuid.UUID) []stmt {
			return []stmt{commentRow(f, sid, with(proposalOf(), map[string]any{"session_id": nil}))}
		}, "variance_comment_session"},
		{"a person's comment citing", func(f fixture, sid uuid.UUID) []stmt {
			return []stmt{commentRow(f, sid, map[string]any{"citations": `[{"kind": "payment", "id": "` + id().String() + `"}]`})}
		}, "variance_comment_session"},
		{"citations no list", func(f fixture, sid uuid.UUID) []stmt {
			return []stmt{commentRow(f, sid, with(proposalOf(), map[string]any{"citations": `{}`}))}
		}, "variance_comment_citations"},
		{"51 citations", func(f fixture, sid uuid.UUID) []stmt {
			return []stmt{commentRow(f, sid, with(proposalOf(), map[string]any{"citations": "[" + strings.Repeat(`{}, `, 50) + "{}]"}))}
		}, "variance_comment_citations"},
		{"rejected without a reason", func(f fixture, sid uuid.UUID) []stmt {
			return []stmt{commentRow(f, sid, with(proposalOf(), map[string]any{"status": "rejected", "decided_by": id(), "decided_at": "2026-04-24T10:00:00Z"}))}
		}, "variance_comment_rejected"},
		{"a reason not rejected", func(f fixture, sid uuid.UUID) []stmt {
			return []stmt{commentRow(f, sid, map[string]any{"decision_reason": "Why."})}
		}, "variance_comment_rejected"},
		{"a blank reason", func(f fixture, sid uuid.UUID) []stmt {
			return []stmt{commentRow(f, sid, with(proposalOf(), map[string]any{"status": "rejected", "decided_by": id(), "decided_at": "2026-04-24T10:00:00Z",
				"decision_reason": ""}))}
		}, "variance_comment_rejected"},
		{"version 0", func(f fixture, sid uuid.UUID) []stmt { return []stmt{commentRow(f, sid, map[string]any{"version": 0})} },
			"variance_comment_version"},
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
		{"a proposal decided by another", func(f fixture, sid uuid.UUID) []stmt {
			cid := id()
			return []stmt{commentRowID(f, sid, cid, proposalOf()), decide(cid, "'accepted'", "version + 1", id())}
		}, ""},
		{"a proposal decided twice", func(f fixture, sid uuid.UUID) []stmt {
			cid := id()
			return []stmt{commentRowID(f, sid, cid, proposalOf()), decide(cid, "'accepted'", "version + 1", id()), decide(cid, "'accepted'", "version + 1", id())}
		}, "only a proposal is decided, once"},
		{"a person's comment decided", func(f fixture, sid uuid.UUID) []stmt {
			cid := id()
			return []stmt{commentRowID(f, sid, cid, nil), decide(cid, "'accepted'", "version + 1", id())}
		}, "only a proposal is decided, once"},
		{"a proposal left proposed", func(f fixture, sid uuid.UUID) []stmt {
			cid := id()
			return []stmt{commentRowID(f, sid, cid, proposalOf()), decide(cid, "'proposed'", "version + 1", nil)}
		}, "only a proposal is decided, once"},
		{"a decision keeping the version", func(f fixture, sid uuid.UUID) []stmt {
			cid := id()
			return []stmt{commentRowID(f, sid, cid, proposalOf()), decide(cid, "'accepted'", "version", id())}
		}, "a decision raises the version by one"},
		{"a proposal decided by its author", func(f fixture, sid uuid.UUID) []stmt {
			cid := id()
			return []stmt{commentRowID(f, sid, cid, proposalOf()), decide(cid, "'accepted'", "version + 1", f.actor)}
		}, "its author does not decide its own proposal"},
		{"a proposal accepted edited, keeping the agent's", func(f fixture, sid uuid.UUID) []stmt {
			cid := id()
			return []stmt{commentRowID(f, sid, cid, proposalOf()), step(`UPDATE cash.variance_comment SET status = 'accepted', version = version + 1,
				decided_by = $2, decided_at = now(), text = 'Mine.', proposed_text = text WHERE id = $1`, cid, id())}
		}, ""},
		{"an edit losing the agent's text", func(f fixture, sid uuid.UUID) []stmt {
			cid := id()
			return []stmt{commentRowID(f, sid, cid, proposalOf()), step(`UPDATE cash.variance_comment SET status = 'accepted', version = version + 1,
				decided_by = $2, decided_at = now(), text = 'Mine.' WHERE id = $1`, cid, id())}
		}, "only an accepted edit changes the text"},
		{"a rejection editing", func(f fixture, sid uuid.UUID) []stmt {
			cid := id()
			return []stmt{commentRowID(f, sid, cid, proposalOf()), step(`UPDATE cash.variance_comment SET status = 'rejected', version = version + 1,
				decided_by = $2, decided_at = now(), decision_reason = 'No.', text = 'Mine.', proposed_text = text WHERE id = $1`, cid, id())}
		}, "only an accepted edit changes the text"},
		{"a proposed text kept unedited", func(f fixture, sid uuid.UUID) []stmt {
			cid := id()
			return []stmt{commentRowID(f, sid, cid, proposalOf()), step(`UPDATE cash.variance_comment SET status = 'accepted', version = version + 1,
				decided_by = $2, decided_at = now(), proposed_text = 'Other.' WHERE id = $1`, cid, id())}
		}, "only an accepted edit changes the text"},
		{"a person's comment with a proposed text", func(f fixture, sid uuid.UUID) []stmt {
			return []stmt{commentRow(f, sid, map[string]any{"proposed_text": "Other."})}
		}, "variance_comment_edited"},
		{"a comment changed", func(f fixture, sid uuid.UUID) []stmt {
			return []stmt{commentRow(f, sid, nil), step(`UPDATE cash.variance_comment SET text = 'Other.' WHERE snapshot_id = $1`, sid)}
		}, "only a proposal is decided, once"},
		{"what a comment explains changed", func(f fixture, sid uuid.UUID) []stmt {
			return []stmt{commentRow(f, sid, nil), step(`UPDATE cash.variance_comment SET lag = 7 WHERE snapshot_id = $1`, sid)}
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

// with is the columns of a with those of b over them.
func with(a, b map[string]any) map[string]any {
	for k, v := range b {
		a[k] = v
	}
	return a
}

// decide decides the comment: its status and version as SQL, by whom.
func decide(cid uuid.UUID, status, version string, by any) stmt {
	return step(`UPDATE cash.variance_comment SET status = `+status+`, version = `+version+`, decided_by = $2,
		decided_at = CASE WHEN $2::uuid IS NULL THEN NULL ELSE now() END WHERE id = $1`, cid, by)
}

// TestCashVarianceCommentPrivileges: the comment and proposal permissions
// are seeded and are no approvals; the application may insert and read
// comments and decide a proposal (its status, who decided, when, why and
// the version), never change what a comment says or delete one (T53f4,
// 000201; T53i, 000218).
func TestCashVarianceCommentPrivileges(t *testing.T) {
	need(t)
	for _, code := range []string{"cash.variance.comment", "cash.variance.propose"} {
		var approval bool
		if err := testDB.QueryRow(context.Background(), `SELECT is_approval FROM ref.permission WHERE code = $1`, code).Scan(&approval); err != nil {
			t.Fatal(err)
		}
		if approval {
			t.Errorf("%s: got an approval permission, want none", code)
		}
	}
	for _, c := range []struct {
		check string
		want  bool
	}{
		{`has_table_privilege('erp_app', 'cash.variance_comment', 'INSERT')`, true},
		{`has_table_privilege('erp_app', 'cash.variance_comment', 'UPDATE')`, false},
		{`has_column_privilege('erp_app', 'cash.variance_comment', 'status', 'UPDATE')`, true},
		{`has_column_privilege('erp_app', 'cash.variance_comment', 'decision_reason', 'UPDATE')`, true},
		{`has_column_privilege('erp_app', 'cash.variance_comment', 'version', 'UPDATE')`, true},
		{`has_column_privilege('erp_app', 'cash.variance_comment', 'text', 'UPDATE')`, true},
		{`has_column_privilege('erp_app', 'cash.variance_comment', 'proposed_text', 'UPDATE')`, true},
		{`has_column_privilege('erp_app', 'cash.variance_comment', 'lag', 'UPDATE')`, false},
		{`has_column_privilege('erp_app', 'cash.variance_comment', 'citations', 'UPDATE')`, false},
		{`has_column_privilege('erp_app', 'cash.variance_comment', 'session_id', 'UPDATE')`, false},
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
