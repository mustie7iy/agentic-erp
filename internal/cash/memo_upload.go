package cash

import (
	"bytes"
	"cmp"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// An upload (ADR-0051 §7) is a CSV file of memo records under a batch key:
// a batch replaces the records of its key that are not realised, by
// reference, so the same file again changes nothing. Every row is checked
// before anything is written; one wrong row refuses the batch, naming each
// wrong field by its line.

// Bounds of an upload.
const (
	maxUploadBytes = 1 << 20
	maxUploadRows  = 5000
	maxRowErrors   = 100
)

// uploadColumns are the file's columns, true for the required ones.
var uploadColumns = map[string]bool{"reference": true, "value_date": true, "amount": true, "currency": true, "liquidity_item": true,
	"certainty": false, "house_bank": false, "expires_on": false, "description": false}

// uploadFields are the columns a record's field is read from.
var uploadFields = map[string]string{"amount.amount": "amount", "amount.currency": "currency", "house_bank_id": "house_bank"}

// UploadResult is what an upload did to the records of its batch key.
type UploadResult struct {
	Rows, Created, Changed, Unchanged, Cancelled, KeptRealised int
}

// uploadRow is a row of the file: its line and its values by column.
type uploadRow struct {
	line   int
	values map[string]string
}

func uploadInvalid(errs ...problem.FieldError) error {
	return problem.Unprocessable("cash.memo_upload_invalid", "The file's rows are not memo records; nothing was written.", errs...)
}

// parseUpload reads the file's header and rows.
func parseUpload(file []byte) ([]uploadRow, error) {
	if len(file) > maxUploadBytes || !utf8.Valid(file) {
		return nil, uploadInvalid(problem.FieldError{Field: "file", Code: "invalid", Message: "a UTF-8 CSV file of at most 1 MB"})
	}
	r := csv.NewReader(bytes.NewReader(bytes.TrimPrefix(file, []byte{0xEF, 0xBB, 0xBF})))
	header, err := r.Read()
	if err != nil {
		return nil, uploadInvalid(problem.FieldError{Field: "file", Code: "invalid", Message: "a header row naming the columns: " + err.Error()})
	}
	if err := checkHeader(header); err != nil {
		return nil, err
	}
	var out []uploadRow
	for {
		rec, err := r.Read()
		switch {
		case errors.Is(err, io.EOF):
			return out, nil
		case err != nil: // a csv.ParseError, naming its line
			return nil, uploadInvalid(problem.FieldError{Field: "file", Code: "invalid", Message: err.Error()})
		case len(out) == maxUploadRows:
			return nil, uploadInvalid(problem.FieldError{Field: "file", Code: "invalid", Message: fmt.Sprintf("at most %d rows", maxUploadRows)})
		}
		line, _ := r.FieldPos(0)
		row := uploadRow{line: line, values: map[string]string{}}
		for i, v := range rec {
			row.values[header[i]] = strings.TrimSpace(v)
		}
		out = append(out, row)
	}
}

// checkHeader refuses unknown, repeated and missing columns.
func checkHeader(header []string) error {
	seen := map[string]bool{}
	var errs []problem.FieldError
	for i, h := range header {
		h = strings.TrimSpace(h)
		header[i] = h
		if _, ok := uploadColumns[h]; !ok || seen[h] {
			errs = append(errs, problem.FieldError{Field: "file", Code: "invalid", Message: fmt.Sprintf("column %q is unknown or repeated", h)})
		}
		seen[h] = true
	}
	for c, required := range uploadColumns {
		if required && !seen[c] {
			errs = append(errs, problem.FieldError{Field: "file", Code: "invalid", Message: fmt.Sprintf("column %q is required", c)})
		}
	}
	if len(errs) > 0 {
		return uploadInvalid(errs...)
	}
	return nil
}

// input reads a row as a record's input, with the house bank by its code.
func (c *checker) input(row uploadRow) (MemoInput, []problem.FieldError) {
	v := row.values
	in := MemoInput{LegalEntityID: c.legalEntity, Amount: v["amount"], Currency: v["currency"], Certainty: v["certainty"], Item: v["liquidity_item"],
		Reference: new(v["reference"])}
	var errs []problem.FieldError
	bad := func(column, message string) {
		errs = append(errs, problem.FieldError{Field: fmt.Sprintf("rows[%d].%s", row.line, column), Code: "invalid", Message: message})
	}
	var err error
	if in.ValueDate, err = time.Parse(time.DateOnly, v["value_date"]); err != nil {
		bad("value_date", "a day, YYYY-MM-DD")
	}
	if s := v["expires_on"]; s != "" {
		d, err := time.Parse(time.DateOnly, s)
		in.ExpiresOn = &d
		if err != nil {
			bad("expires_on", "a day, YYYY-MM-DD")
		}
	}
	if code := v["house_bank"]; code != "" {
		in.HouseBankID = c.bankByCode(code)
		if in.HouseBankID == nil {
			bad("house_bank", "the code of an active house bank of the legal entity")
		}
	}
	if s := v["description"]; s != "" {
		in.Description = &s
	}
	if n := utf8.RuneCountInString(v["reference"]); n == 0 || n > 100 {
		bad("reference", "1 to 100 characters")
	}
	if utf8.RuneCountInString(v["description"]) > 500 {
		bad("description", "at most 500 characters")
	}
	return in, errs
}

func (c *checker) bankByCode(code string) *uuid.UUID {
	for id, b := range c.banks {
		if b.Code == code {
			return &id
		}
	}
	return nil
}

// rowValues checks every row, reporting each wrong field by its line.
func (c *checker) rowValues(ctx context.Context, rows []uploadRow) ([]memoValues, error) {
	out := make([]memoValues, len(rows))
	var errs []problem.FieldError
	refs := map[string]int{}
	for i, row := range rows {
		in, bad := c.input(row)
		if first, ok := refs[*in.Reference]; ok {
			bad = append(bad, problem.FieldError{Field: fmt.Sprintf("rows[%d].reference", row.line), Code: "invalid",
				Message: fmt.Sprintf("in the file once; line %d has it", first)})
		}
		refs[*in.Reference] = row.line
		if len(bad) == 0 {
			v, err := c.values(ctx, in)
			var p *problem.Problem
			switch {
			case err == nil:
				out[i] = v
			case errors.As(err, &p):
				for _, fe := range p.Errors {
					fe.Field = fmt.Sprintf("rows[%d].%s", row.line, cmp.Or(uploadFields[fe.Field], fe.Field))
					bad = append(bad, fe)
				}
			default:
				return nil, err
			}
		}
		errs = append(errs, bad...)
	}
	if len(errs) > 0 {
		return nil, uploadInvalid(errs[:min(len(errs), maxRowErrors)]...)
	}
	return out, nil
}

// UploadMemos applies a batch of memo records of the legal entity under
// its key: the file's references are created, changed or left as they
// are, realised ones kept, and the key's active records whose reference
// the file no longer has cancelled.
func (e *Endpoints) UploadMemos(ctx context.Context, legalEntity uuid.UUID, batchKey string, file []byte) (UploadResult, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return UploadResult{}, err
	}
	c, err := e.checker(ctx, tx, legalEntity)
	if err != nil {
		return UploadResult{}, err
	}
	rows, err := parseUpload(file)
	if err != nil {
		return UploadResult{}, err
	}
	values, err := c.rowValues(ctx, rows)
	if err != nil {
		return UploadResult{}, err
	}
	existing, err := batch(ctx, tx, legalEntity, batchKey)
	if err != nil {
		return UploadResult{}, err
	}
	u := upload{tx: tx, p: p, legalEntity: legalEntity, batchKey: batchKey, existing: existing, result: UploadResult{Rows: len(rows)}}
	for _, v := range values {
		if err := u.apply(ctx, v); err != nil {
			return UploadResult{}, err
		}
	}
	return u.result, u.cancelRest(ctx)
}

// batch locks the key's records by reference; the key's lock serializes
// uploads of it, so one finds what the other wrote.
func batch(ctx context.Context, tx pgx.Tx, legalEntity uuid.UUID, key string) (map[string]MemoRecord, error) {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('cash.memo_upload:' || platform.require_tenant() || ':' || $1::text || ':' || $2, 0))`,
		legalEntity, key); err != nil {
		return nil, fmt.Errorf("cash: lock batch %s: %w", key, err)
	}
	rows, _ := tx.Query(ctx, memoSelect+` WHERE m.legal_entity_id = $1 AND m.batch_key = $2 ORDER BY m.reference FOR UPDATE OF m`, legalEntity, key)
	ms, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (MemoRecord, error) { return scanMemo(r) })
	if err != nil {
		return nil, fmt.Errorf("cash: memo records of batch %s: %w", key, err)
	}
	out := make(map[string]MemoRecord, len(ms))
	for _, m := range ms {
		out[*m.Reference] = m
	}
	return out, nil
}

// upload is a batch being applied.
type upload struct {
	tx          pgx.Tx
	p           reqctx.Principal
	legalEntity uuid.UUID
	batchKey    string
	existing    map[string]MemoRecord // by reference; applied ones removed
	result      UploadResult
}

// apply writes a row: created for a new reference, changed when its record
// differs or was cancelled, left when it is the same or realised.
func (u *upload) apply(ctx context.Context, v memoValues) error {
	ref := *v.reference
	m, ok := u.existing[ref]
	delete(u.existing, ref)
	switch {
	case !ok:
		u.result.Created++
		id, err := insertMemo(ctx, u.tx, u.p, u.legalEntity, v, memoMeta{status: MemoActive, origin: OriginUpload, batchKey: &u.batchKey})
		if err != nil {
			return err
		}
		_, err = written(ctx, u.tx, id, "create", "created")
		return err
	case m.Status == MemoRealised:
		u.result.KeptRealised++
		return nil
	case m.Status == MemoActive && same(m, v):
		u.result.Unchanged++
		return nil
	}
	u.result.Changed++
	if err := updateValues(ctx, u.tx, u.p, m.ID, v, true); err != nil {
		return err
	}
	_, err := written(ctx, u.tx, m.ID, "update", "updated")
	return err
}

// cancelRest cancels the key's active records the file no longer has.
func (u *upload) cancelRest(ctx context.Context) error {
	comment := "Not in the latest upload of batch " + u.batchKey + "."
	for _, m := range u.existing {
		if m.Status != MemoActive {
			continue
		}
		u.result.Cancelled++
		if err := decide(ctx, u.tx, u.p, m.ID, MemoCancelled, &comment, nil); err != nil {
			return err
		}
		if _, err := written(ctx, u.tx, m.ID, "cancel", MemoCancelled); err != nil {
			return err
		}
	}
	return nil
}

// same reports whether the record already has the row's values.
func same(m MemoRecord, v memoValues) bool {
	return eq(m.HouseBankID, v.houseBank) && m.Currency == v.currency && m.Amount == v.amount && m.ValueDate.Equal(v.valueDate) &&
		m.ExpiresOn.Equal(v.expiresOn) && m.Certainty == v.certainty && m.Item == v.item && eq(m.Description, v.description)
}

func eq[T comparable](a, b *T) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}
