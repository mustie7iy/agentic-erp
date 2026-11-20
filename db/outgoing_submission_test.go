package db

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// submissionRow inserts a queued submission of the scene's valid
// e-invoice (migration 000421_outgoing_submissions; ADR-0061 As built E3):
// $1 tenant, $2 id, $3 e-invoice, $4 business type, $5 actor.
const submissionRow = `INSERT INTO einvoicing.outgoing_submission (tenant_id, id, outgoing_einvoice_id, legal_entity_id, connector_id, operation_id,
	filename, business_type, status, next_check_at, requested_by, created_by, updated_by)
	VALUES ($1, $2, $3, gen_random_uuid(), gen_random_uuid(), gen_random_uuid(), 'CI100001', $4, 'queued', now(), $5, $5, $5)`

// TestOutgoingSubmissionPermission: the send permission is seeded, not an
// approval permission.
func TestOutgoingSubmissionPermission(t *testing.T) {
	need(t)
	var n int
	if err := testDB.QueryRow(context.Background(), `SELECT count(*) FROM ref.permission WHERE code = 'einvoicing.outgoing.send' AND NOT is_approval`).
		Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("got %d, want 1", n)
	}
}

// TestOutgoingSubmissionRules: what the database refuses of a submission,
// as the application role: one per e-invoice, of an e-invoice that
// exists; B2B or B2G; a tracking id once submitted or completed; its next
// check exactly while in progress; a problem exactly when failed; a
// completion time exactly when completed; the moves the send makes and
// no others; what names the e-invoice fixed; the columns' forms; never
// deleted.
func TestOutgoingSubmissionRules(t *testing.T) {
	need(t)
	update := func(set string) stmt {
		return step(`UPDATE einvoicing.outgoing_submission SET ` + set + `, version = version + 1`)
	}
	queued := func(s outgoingScene) []stmt {
		return []stmt{s.valid(), step(submissionRow, s.tenant, id(), s.doc, "B2B", s.actor)}
	}
	with := func(s outgoingScene, more ...stmt) []stmt { return append(queued(s), more...) }
	const sending = `status = 'sending', attempts = 1`
	const submitted = `status = 'submitted', tracking_id = 'T-1', provider_status = 'PENDING'`
	const failed = `status = 'failed', next_check_at = NULL, problem_code = 'einvoicing.sending_failed', problem_detail = 'd'`
	cases := []struct {
		name  string
		stmts func(s outgoingScene) []stmt
		want  string // empty: allowed
	}{
		{"queued, sent, submitted and completed", func(s outgoingScene) []stmt {
			return with(s, update(sending), update(submitted), update(`polls = 1`),
				update(`status = 'completed', provider_status = 'COMPLETED', next_check_at = NULL, completed_at = now()`))
		}, ""},
		{"a lapsed claim claimed again", func(s outgoingScene) []stmt { return with(s, update(sending), update(`attempts = 2`)) }, ""},
		{"sent again after SENDING_FAILED", func(s outgoingScene) []stmt {
			return with(s, update(sending), update(submitted), update(`status = 'queued', failed_tracking_ids = ARRAY['T-1'], resends = 1`))
		}, ""},
		{"failed and queued again", func(s outgoingScene) []stmt {
			return with(s, update(failed), update(`status = 'queued', next_check_at = now(), problem_code = NULL, problem_detail = NULL`))
		}, ""},
		{"a second submission of an e-invoice", func(s outgoingScene) []stmt {
			return with(s, step(submissionRow, s.tenant, id(), s.doc, "B2G", s.actor))
		}, "outgoing_submission_once"},
		{"of no e-invoice", func(s outgoingScene) []stmt { return []stmt{step(submissionRow, s.tenant, id(), id(), "B2B", s.actor)} },
			"outgoing_submission_einvoice"},
		{"B2C", func(s outgoingScene) []stmt {
			return []stmt{s.valid(), step(submissionRow, s.tenant, id(), s.doc, "B2C", s.actor)}
		},
			"outgoing_submission_business_type_check"},
		{"submitted without a tracking id", func(s outgoingScene) []stmt { return with(s, update(sending), update(`status = 'submitted'`)) },
			"outgoing_submission_tracked"},
		{"in progress without its next check", func(s outgoingScene) []stmt { return with(s, update(sending+`, next_check_at = NULL`)) }, "outgoing_submission_due"},
		{"failed without its problem", func(s outgoingScene) []stmt { return with(s, update(`status = 'failed', next_check_at = NULL`)) },
			"outgoing_submission_problem"},
		{"a problem's code without its detail", func(s outgoingScene) []stmt {
			return with(s, update(`status = 'failed', next_check_at = NULL, problem_code = 'einvoicing.x'`))
		}, "outgoing_submission_problem"},
		{"completed without its time", func(s outgoingScene) []stmt {
			return with(s, update(sending), update(submitted), update(`status = 'completed', next_check_at = NULL`))
		}, "outgoing_submission_completed"},
		{"queued straight to submitted", func(s outgoingScene) []stmt { return with(s, update(submitted)) }, "does not move from"},
		{"queued to queued", func(s outgoingScene) []stmt { return with(s, update(`attempts = 1`)) }, "does not move from"},
		{"completed moved", func(s outgoingScene) []stmt {
			return with(s, update(sending), update(submitted), update(`status = 'completed', next_check_at = NULL, completed_at = now()`),
				update(`status = 'queued', next_check_at = now(), completed_at = NULL`))
		}, "does not move from"},
		{"failed to submitted", func(s outgoingScene) []stmt {
			return with(s, update(failed), update(`status = 'submitted', tracking_id = 'T', next_check_at = now(), problem_code = NULL, problem_detail = NULL`))
		}, "does not move from"},
		{"moved to another e-invoice", func(s outgoingScene) []stmt { return with(s, update(sending+`, filename = 'OTHER'`)) }, "keeps its e-invoice"},
		{"a filename empty", func(s outgoingScene) []stmt {
			return []stmt{s.valid(), step(strings.Replace(submissionRow, "'CI100001'", "''", 1), s.tenant, id(), s.doc, "B2B", s.actor)}
		}, "outgoing_submission_filename_check"},
		{"a provider status of another form", func(s outgoingScene) []stmt { return with(s, update(sending+`, provider_status = 'pending'`)) },
			"outgoing_submission_provider_status_check"},
		{"a tracking id over 200 characters", func(s outgoingScene) []stmt { return with(s, update(sending+`, tracking_id = repeat('t', 201)`)) },
			"outgoing_submission_tracking_id_check"},
		{"attempts below zero", func(s outgoingScene) []stmt { return with(s, update(`status = 'sending', attempts = -1`)) }, "outgoing_submission_attempts_check"},
		{"resends below zero", func(s outgoingScene) []stmt { return with(s, update(sending+`, resends = -1`)) }, "outgoing_submission_resends_check"},
		{"polls below zero", func(s outgoingScene) []stmt { return with(s, update(sending+`, polls = -1`)) }, "outgoing_submission_polls_check"},
		{"a problem code of another form", func(s outgoingScene) []stmt {
			return with(s, update(`status = 'failed', next_check_at = NULL, problem_code = 'Bad', problem_detail = 'd'`))
		}, "outgoing_submission_problem_code_check"},
		{"a problem detail over 1000 characters", func(s outgoingScene) []stmt {
			return with(s, update(`status = 'failed', next_check_at = NULL, problem_code = 'einvoicing.x', problem_detail = repeat('d', 1001)`))
		}, "outgoing_submission_problem_detail_check"},
		{"a submission deleted", func(s outgoingScene) []stmt { return with(s, step(`DELETE FROM einvoicing.outgoing_submission`)) }, "permission denied"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := outgoingScene{tenant: id(), actor: id(), doc: id(), invoice: id()}
			err := asApp(t, s.tenant, func(tx pgx.Tx) error { return all(tx, c.stmts(s)...) })
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

// TestOutgoingSubmissionsDownRefused: 000421's down migration refuses to
// drop submissions (06 §6.4: a down migration loses no data).
func TestOutgoingSubmissionsDownRefused(t *testing.T) {
	need(t)
	ctx := context.Background()
	tx, err := testDB.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	s := outgoingScene{tenant: id(), actor: id(), doc: id(), invoice: id()}
	if err := all(tx, step(`SELECT set_config('app.tenant_id', $1, true)`, s.tenant.String()), s.valid(), step(submissionRow, s.tenant, id(), s.doc, "B2B", s.actor),
		step(`SELECT set_config('app.tenant_id', '', true)`), step(`SAVEPOINT owner`), step(`SET LOCAL ROLE erp_owner`)); err != nil {
		t.Fatal(err)
	}
	down := migrationNamed(t, "000421_outgoing_submissions").Down
	failsWith(t, exec(tx, down), "row-level security")
	if err := all(tx, step(`ROLLBACK TO SAVEPOINT owner`), step(`RESET ROLE`)); err != nil {
		t.Fatal(err)
	}
	failsWith(t, exec(tx, down), "e-invoice submissions exist")
}
