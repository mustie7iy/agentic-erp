package audit

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/jsonl"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// EvidenceFormat names the form of an evidence export's files and
// manifest (ADR-0011 As built), which the manifest states.
const EvidenceFormat = "agentic-erp.audit.evidence.v1"

// The files of an export, in the manifest's order, and the manifest.
const (
	auditEventsName    = "audit_events.jsonl"
	journalEntriesName = "journal_entries.jsonl"
	decisionsName      = "approval_decisions.jsonl"
	manifestName       = "manifest.json"
)

var fileNames = []string{auditEventsName, journalEntriesName, decisionsName}

// fileKey is the name in the store of an export's file or a part of one:
// under the export's id, by the file it belongs to and named by its
// content's SHA-256 too, so that a round run again after a lost commit,
// which may read more of a period still open, writes its files beside the
// first round's instead of over them, and a part's key needs no number;
// the manifest names the ones it holds.
func fileKey(export uuid.UUID, name string, sum [32]byte) string {
	stem, _, _ := strings.Cut(name, ".")
	return export.String() + "/" + stem + "." + hex.EncodeToString(sum[:]) + ".jsonl"
}

// fileOf is the file a file's or a part's name belongs to:
// audit_events.part-2.jsonl is audit_events.jsonl's.
func fileOf(name string) string {
	stem, _, _ := strings.Cut(name, ".")
	return stem + ".jsonl"
}

// partName is the name of the n-th part, from 1, of a file written in
// parts.
func partName(file string, n int) string {
	stem, _, _ := strings.Cut(file, ".")
	return fmt.Sprintf("%s.part-%d.jsonl", stem, n)
}

// manifestKey is where the export's manifest is kept, once.
func manifestKey(tenant, export uuid.UUID) string {
	return blobstore.Key(tenant, blobstore.KindEvidence, export.String()+"/"+manifestName)
}

// eventRecord is an audit event as a line of audit_events.jsonl: as the
// trail stores it, its detail as stored, without its hashes, and its
// client address only in an export made with them (null for work no
// request made).
type eventRecord struct {
	Seq           int64           `json:"seq"`
	OccurredAt    string          `json:"occurred_at"`
	ActorID       uuid.UUID       `json:"actor_id"`
	ActorKind     string          `json:"actor_kind"`
	Action        string          `json:"action"`
	ObjectType    string          `json:"object_type"`
	ObjectID      *uuid.UUID      `json:"object_id"`
	Outcome       string          `json:"outcome"`
	Detail        json.RawMessage `json:"detail"`
	TraceID       *string         `json:"trace_id"`
	ClientAddress json.RawMessage `json:"client_address,omitempty"`
}

// decisionRecord is a decision on an approval request about a journal
// entry's source document, as a line of approval_decisions.jsonl, with
// who decided.
type decisionRecord struct {
	ApprovalRequestID uuid.UUID   `json:"approval_request_id"`
	ObjectType        string      `json:"object_type"`
	ObjectID          uuid.UUID   `json:"object_id"`
	JournalEntryIDs   []uuid.UUID `json:"journal_entry_ids"`
	Title             string      `json:"title"`
	RequestStatus     string      `json:"request_status"`
	DecisionID        uuid.UUID   `json:"decision_id"`
	StepNo            int16       `json:"step_no"`
	Decision          string      `json:"decision"`
	DecidedBy         uuid.UUID   `json:"decided_by"`
	DecidedByName     string      `json:"decided_by_name"`
	Comment           *string     `json:"comment"`
	DecidedAt         string      `json:"decided_at"`
}

// manifestCompany is a company the export covers.
type manifestCompany struct {
	LegalEntityID uuid.UUID `json:"legal_entity_id"`
	CompanyCode   string    `json:"company_code"`
}

// manifestChain is the verification of the tenant's chain from its first
// event to its head.
type manifestChain struct {
	Verified       bool  `json:"verified"`
	FromSeq        int64 `json:"from_seq"`
	LastSeq        int64 `json:"last_seq"`
	Checked        int64 `json:"checked"`
	AnchorsChecked int32 `json:"anchors_checked"`
	FirstBrokenSeq int64 `json:"first_broken_seq,omitempty"`
}

// evidenceManifest describes an export (ADR-0011 As built): what it
// covers, each file with its SHA-256, size and records, and the chain's
// verification.
type evidenceManifest struct {
	Format              string            `json:"format"`
	EvidenceExportID    uuid.UUID         `json:"evidence_export_id"`
	Companies           []manifestCompany `json:"companies"`
	From                string            `json:"from"`
	To                  string            `json:"to"`
	AuditEventsFrom     string            `json:"audit_events_from"`
	AuditEventsBefore   string            `json:"audit_events_before"`
	WithClientAddresses bool              `json:"with_client_addresses"`
	RequestedBy         uuid.UUID         `json:"requested_by"`
	RequestedAt         string            `json:"requested_at"`
	Files               []ExportFile      `json:"files"`
	Chain               manifestChain     `json:"chain"`
}

// The most records, and bytes, a part of an export's file holds
// (ADR-0011 As built, review:batch75): a file is built and written to the
// store a part at a time, so an export of any size holds one part of each
// file in memory, and a part is downloaded whole as a file of its own. A
// file that fits one part is that part, under the file's name.
const (
	EvidencePartRecords = 50_000
	EvidencePartBytes   = 32 << 20
)

// fileParts writes one of an export's files in parts, each kept in the
// store as it fills, and lists them as the manifest names them.
type fileParts struct {
	name  string
	files []ExportFile
	w     *jsonl.Writer
}

// parts starts the export's file of the name.
func (e *Endpoints) parts(ctx context.Context, tenant, export uuid.UUID, name string) *fileParts {
	f := &fileParts{name: name}
	f.w = jsonl.New(cmp.Or(e.PartRecords, EvidencePartRecords), EvidencePartBytes, func(content []byte, records int32) error {
		sum := sha256.Sum256(content)
		if err := e.keep(ctx, blobstore.Key(tenant, blobstore.KindEvidence, fileKey(export, name, sum)), content); err != nil {
			return err
		}
		f.files = append(f.files, ExportFile{Name: name, SHA256: hex.EncodeToString(sum[:]), SizeBytes: int64(len(content)), Records: records})
		return nil
	})
	return f
}

// close writes the file's last part and returns its parts: one, under the
// file's name, or several, each named and numbered as a part.
func (f *fileParts) close() ([]ExportFile, error) {
	if err := f.w.Close(); err != nil {
		return nil, err
	}
	if len(f.files) > 1 {
		for i := range f.files {
			f.files[i].Name, f.files[i].Part = partName(f.name, i+1), int32(i+1) // #nosec G115 -- a part number, at most a file's records
		}
	}
	return f.files, nil
}

// build writes the export's files, a part at a time, and returns its
// manifest: the chain's verification to its head, the period's audit
// events up to that head, the companies' journal entries and the
// decisions on their source documents.
func (e *Endpoints) build(ctx context.Context, tx pgx.Tx, tenant uuid.UUID, x Export) ([]byte, error) {
	res, anchors, err := verified(ctx, tx, e.Objects, tenant, 1, 0)
	if err != nil {
		return nil, fmt.Errorf("audit: verify the chain for evidence export %s: %w", x.ID, err)
	}
	from, before := x.From.UTC(), x.To.UTC().AddDate(0, 0, 1)
	events, entries, decisions := e.parts(ctx, tenant, x.ID, auditEventsName), e.parts(ctx, tenant, x.ID, journalEntriesName),
		e.parts(ctx, tenant, x.ID, decisionsName)
	if err := periodEvents(ctx, tx, tenant, from, before, res.LastSeq, x.WithClientAddresses, events.w); err != nil {
		return nil, fmt.Errorf("audit: evidence export %s: %w", x.ID, err)
	}
	sources, err := e.journal(ctx, tx, x, entries.w)
	if err != nil {
		return nil, fmt.Errorf("audit: evidence export %s: %w", x.ID, err)
	}
	if err := e.decisions(ctx, sources, decisions.w); err != nil {
		return nil, fmt.Errorf("audit: evidence export %s: %w", x.ID, err)
	}
	m := evidenceManifest{Format: EvidenceFormat, EvidenceExportID: x.ID, From: x.From.Format(time.DateOnly), To: x.To.Format(time.DateOnly),
		AuditEventsFrom: from.Format(time.RFC3339), AuditEventsBefore: before.Format(time.RFC3339), WithClientAddresses: x.WithClientAddresses,
		RequestedBy: x.RequestedBy, RequestedAt: x.CreatedAt.UTC().Format(time.RFC3339Nano),
		Chain: manifestChain{Verified: res.BrokenAt == 0, FromSeq: 1, LastSeq: res.LastSeq, Checked: res.Checked,
			AnchorsChecked: int32(anchors), FirstBrokenSeq: res.BrokenAt}} // #nosec G115 -- a count of a tenant's anchor rows
	for _, c := range x.Companies {
		m.Companies = append(m.Companies, manifestCompany{LegalEntityID: c.ID, CompanyCode: c.Code})
	}
	for _, f := range []*fileParts{events, entries, decisions} {
		files, err := f.close()
		if err != nil {
			return nil, fmt.Errorf("audit: evidence export %s: %w", x.ID, err)
		}
		m.Files = append(m.Files, files...)
	}
	manifest, _ := json.MarshalIndent(m, "", "  ") // strings, numbers, booleans and ids: it always marshals
	return manifest, nil
}

// periodEvents is the tenant's audit events that occurred from one
// instant to before another, up to the chain's head, by sequence number.
func periodEvents(ctx context.Context, tx pgx.Tx, tenant uuid.UUID, from, before time.Time, head int64, addresses bool, out *jsonl.Writer) error {
	rows, _ := tx.Query(ctx, `SELECT seq, occurred_at, actor_id, actor_kind, action, object_type, object_id, outcome, detail::text, trace_id,
			CASE WHEN $5::boolean THEN host(client_address) END
		FROM audit.audit_event WHERE tenant_id = $1 AND occurred_at >= $2 AND occurred_at < $3 AND seq <= $4
		ORDER BY seq`, tenant, from, before, head, addresses)
	var (
		r       eventRecord
		at      time.Time
		detail  string
		address *string
	)
	_, err := pgx.ForEachRow(rows, []any{&r.Seq, &at, &r.ActorID, &r.ActorKind, &r.Action, &r.ObjectType, &r.ObjectID, &r.Outcome, &detail, &r.TraceID, &address},
		func() error {
			r.OccurredAt, r.Detail, r.ClientAddress = at.UTC().Format(time.RFC3339Nano), json.RawMessage(detail), nil
			if addresses {
				r.ClientAddress, _ = json.Marshal(address) // a string or nil: it always marshals
			}
			return out.Add(r)
		})
	if err != nil {
		return fmt.Errorf("audit events from %s: %w", from.Format(time.DateOnly), err)
	}
	return nil
}

// journal writes the companies' journal entries of the period, company by
// company in the export's order, and returns the entries of each source
// document, in the order the documents first appear: ids only, which the
// decisions are read by.
func (e *Endpoints) journal(ctx context.Context, tx pgx.Tx, x Export, out *jsonl.Writer) (sourceDocs, error) {
	exps, err := currency.Load(ctx, tx)
	if err != nil {
		return sourceDocs{}, fmt.Errorf("journal entries: %w", err)
	}
	sources := sourceDocs{entries: map[uuid.UUID][]uuid.UUID{}}
	for _, c := range x.Companies {
		if err := e.Journal.EntriesBetween(ctx, c.ID, x.From, x.To, func(entry ledger.ArchivedEntry) error {
			sources.add(entry)
			return out.Add(entry.Record(exps))
		}); err != nil {
			return sourceDocs{}, fmt.Errorf("journal entries of company %s: %w", c.Code, err)
		}
	}
	return sources, nil
}

// sourceDocs are the source documents of the exported entries, in the
// order they first appear, with the entries of each.
type sourceDocs struct {
	ids     []uuid.UUID
	entries map[uuid.UUID][]uuid.UUID
}

func (s *sourceDocs) add(entry ledger.ArchivedEntry) {
	if entry.SourceDocumentID == nil {
		return
	}
	id := *entry.SourceDocumentID
	if _, ok := s.entries[id]; !ok {
		s.ids = append(s.ids, id)
	}
	s.entries[id] = append(s.entries[id], entry.ID)
}

// decisions is the decisions on the approval requests about the source
// documents, request by request in the order they were raised, each
// request's in the order they were taken, with who decided.
func (e *Endpoints) decisions(ctx context.Context, sources sourceDocs, out *jsonl.Writer) error {
	if len(sources.ids) == 0 {
		return nil
	}
	requests, err := e.Approvals.About(ctx, sources.ids)
	if err != nil {
		return fmt.Errorf("approval decisions: %w", err)
	}
	var deciders []uuid.UUID
	for _, r := range requests {
		for _, d := range r.Decisions {
			deciders = append(deciders, d.ActorID)
		}
	}
	names, err := e.People.PersonNames(ctx, deciders)
	if err != nil {
		return fmt.Errorf("approval decisions: who decided: %w", err)
	}
	for _, r := range requests {
		for _, d := range r.Decisions {
			if err := out.Add(decisionRecord{ApprovalRequestID: r.ID, ObjectType: r.ObjectType, ObjectID: r.ObjectID, JournalEntryIDs: sources.entries[r.ObjectID],
				Title: r.Title, RequestStatus: r.Status, DecisionID: d.ID, StepNo: d.StepNo, Decision: d.Decision, DecidedBy: d.ActorID,
				DecidedByName: names[d.ActorID], Comment: d.Comment, DecidedAt: d.DecidedAt.UTC().Format(time.RFC3339Nano)}); err != nil {
				return err
			}
		}
	}
	return nil
}

// fileSum is the SHA-256 the manifest names for the file or part. A name
// the manifest does not list is not one of the export's files (404); one
// it lists without a SHA-256 is not handed out unchecked (409).
func fileSum(manifest []byte, name string) ([32]byte, error) {
	var m evidenceManifest
	_ = json.Unmarshal(manifest, &m) // the export's manifest, by its hash: the exporter read it before it completed the export; a manifest naming no file names none
	var names []string
	for _, f := range m.Files {
		names = append(names, f.Name)
		if f.Name != name {
			continue
		}
		var sum [32]byte
		if b, err := hex.DecodeString(f.SHA256); err == nil && len(b) == len(sum) {
			copy(sum[:], b)
			return sum, nil
		}
		return [32]byte{}, problem.Conflict("audit.evidence_changed", "The evidence export's manifest names no file "+name+" with its SHA-256.")
	}
	return [32]byte{}, problem.NotFound("An evidence export has no file " + name + "; its files are " + manifestName + " and " + strings.Join(names, ", ") + ".")
}
