package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/governance"
	"github.com/mustie7iy/agentic-erp/internal/workflow"
)

type CRStatus string

const (
	CRInvalid          CRStatus = "invalid"           // validation errors; cannot proceed
	CRAwaitingEvidence CRStatus = "awaiting_evidence" // tier 3-4: needs simulation and regression results
	CRPendingApproval  CRStatus = "pending_approval"
	CRApproved         CRStatus = "approved"
	CRActive           CRStatus = "active"
	CRRejected         CRStatus = "rejected"
)

// Evidence is a check result attached to a change request, such as a
// sandbox simulation or a regression run.
type Evidence struct {
	Kind    string          `json:"kind"` // simulation | regression
	Passed  bool            `json:"passed"`
	Summary string          `json:"summary"`
	Detail  json.RawMessage `json:"detail,omitempty"`
	At      time.Time       `json:"at"`
}

// RequiredEvidence lists what a tier needs before approval.
func RequiredEvidence(t Tier) []string {
	if t >= TierPostingRelated {
		return []string{"simulation", "regression"}
	}
	return nil
}

type ChangeRequest struct {
	ID               string            `json:"id"`
	Title            string            `json:"title"`
	Rationale        string            `json:"rationale"`
	Author           string            `json:"author"`
	BaseVersion      int               `json:"baseVersion"`
	Proposed         string            `json:"proposed"`
	Changes          []Change          `json:"changes"`
	Tier             Tier              `json:"tier"`
	Findings         []Finding         `json:"findings"`
	Evidence         []Evidence        `json:"evidence,omitempty"`
	Workflow         *workflow.Request `json:"workflow,omitempty"`
	Status           CRStatus          `json:"status"`
	CreatedAt        time.Time         `json:"createdAt"`
	ActivatedVersion int               `json:"activatedVersion,omitempty"`
}

type Version struct {
	Number      int       `json:"number"`
	Hash        string    `json:"hash"`
	ActivatedAt time.Time `json:"activatedAt"`
	ActivatedBy string    `json:"activatedBy"`
	Source      string    `json:"source"` // bootstrap | CR id | rollback to vN
}

type repoState struct {
	Active   int                       `json:"active"`
	Versions []Version                 `json:"versions"`
	Changes  map[string]*ChangeRequest `json:"changes"`
	Seq      int                       `json:"seq"`
}

// Repo stores configuration versions and change requests in a directory:
// versions/vNNNN.yaml plus state.json. It is safe for concurrent use.
type Repo struct {
	mu    sync.Mutex
	dir   string
	state repoState
	audit *governance.AuditLog
	now   func() time.Time
}

var (
	ErrNoActive   = errors.New("no active configuration (bootstrap first)")
	ErrStale      = errors.New("change request is based on an older version; propose it again on the active version")
	ErrPermission = errors.New("permission denied")
)

// OpenRepo opens (or creates) a configuration repository.
func OpenRepo(dir string, audit *governance.AuditLog) (*Repo, error) {
	r := &Repo{dir: dir, audit: audit, now: time.Now, state: repoState{Changes: map[string]*ChangeRequest{}}}
	b, err := os.ReadFile(filepath.Join(dir, "state.json"))
	switch {
	case errors.Is(err, os.ErrNotExist):
		return r, nil
	case err != nil:
		return nil, err
	}
	if err := json.Unmarshal(b, &r.state); err != nil {
		return nil, fmt.Errorf("configuration repository: %w", err)
	}
	if r.state.Changes == nil {
		r.state.Changes = map[string]*ChangeRequest{}
	}
	return r, nil
}

// SetClock replaces the clock (tests).
func (r *Repo) SetClock(now func() time.Time) { r.now = now }

func (r *Repo) versionPath(n int) string {
	return filepath.Join(r.dir, "versions", fmt.Sprintf("v%04d.yaml", n))
}

// Active returns the active configuration and its version number.
func (r *Repo) Active() (*Config, int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.activeLocked()
}

func (r *Repo) activeLocked() (*Config, int, error) {
	if r.state.Active == 0 {
		return nil, 0, ErrNoActive
	}
	c, err := r.loadVersion(r.state.Active)
	return c, r.state.Active, err
}

func (r *Repo) loadVersion(n int) (*Config, error) {
	c, _, err := r.readVersion(n)
	return c, err
}

// readVersion returns a version's parsed configuration and its exact bytes,
// after checking the bytes against the hash recorded at activation.
func (r *Repo) readVersion(n int) (*Config, []byte, error) {
	b, err := os.ReadFile(r.versionPath(n))
	if err != nil {
		return nil, nil, err
	}
	want := r.state.Versions[n-1].Hash
	if got := hash(b); got != want {
		return nil, nil, fmt.Errorf("configuration version %d was modified outside the change process (hash %s, expected %s)", n, got[:12], want[:12])
	}
	c, err := Parse(b)
	return c, b, err
}

// VersionYAML returns the stored YAML of a version.
func (r *Repo) VersionYAML(n int) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n < 1 || n > len(r.state.Versions) {
		return nil, fmt.Errorf("no version %d", n)
	}
	return os.ReadFile(r.versionPath(n))
}

func hash(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// Bootstrap installs the first configuration. It is only allowed while the
// repository is empty, and the configuration must validate cleanly.
func (r *Repo) Bootstrap(yamlDoc []byte, by string) (int, []Finding, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state.Active != 0 {
		return 0, nil, errors.New("repository already has an active configuration; use a change request")
	}
	c, err := Parse(yamlDoc)
	if err != nil {
		return 0, nil, err
	}
	findings := Validate(c)
	if HasErrors(findings) {
		return 0, findings, errors.New("configuration has validation errors")
	}
	n, err := r.writeVersion(yamlDoc, by, "bootstrap")
	if err != nil {
		return 0, findings, err
	}
	r.auditf(by, "config.bootstrap", fmt.Sprintf("v%d", n), "activated", nil)
	return n, findings, r.save()
}

func (r *Repo) writeVersion(doc []byte, by, source string) (int, error) {
	n := len(r.state.Versions) + 1
	if err := os.MkdirAll(filepath.Dir(r.versionPath(n)), 0o755); err != nil {
		return 0, err
	}
	if err := writeAtomic(r.versionPath(n), doc); err != nil {
		return 0, err
	}
	r.state.Versions = append(r.state.Versions, Version{Number: n, Hash: hash(doc), ActivatedAt: r.now().UTC(), ActivatedBy: by, Source: source})
	r.state.Active = n
	return n, nil
}

// Propose creates a change request from a full proposed configuration. The
// author must hold config.propose in the active configuration.
func (r *Repo) Propose(author, title, rationale string, yamlDoc []byte) (*ChangeRequest, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	active, version, err := r.activeLocked()
	if err != nil {
		return nil, err
	}
	id, err := active.Identity(author)
	if err != nil {
		return nil, err
	}
	if !id.Can(governance.ActConfigPropose) {
		return nil, fmt.Errorf("%w: %s lacks %s", ErrPermission, author, governance.ActConfigPropose)
	}
	if title == "" {
		return nil, errors.New("a change request needs a title")
	}
	proposed, err := Parse(yamlDoc)
	if err != nil {
		return nil, err
	}
	changes, _ := Diff(active, proposed) // never fails
	if len(changes) == 0 {
		return nil, errors.New("the proposal does not change anything")
	}
	r.state.Seq++
	cr := &ChangeRequest{
		ID: fmt.Sprintf("CR-%04d", r.state.Seq), Title: title, Rationale: rationale, Author: id.ID,
		BaseVersion: version, Proposed: string(yamlDoc), Changes: changes, Tier: MaxTier(changes),
		Findings: Validate(proposed), CreatedAt: r.now().UTC(),
	}
	// ApprovalSteps always returns well-formed steps, so New cannot fail.
	cr.Workflow, _ = workflow.New("WF-"+cr.ID, "config_change", cr.ID, title, id.ID, 0, ApprovalSteps(cr.Tier), r.now().UTC())
	cr.Status = r.statusFor(cr)
	r.state.Changes[cr.ID] = cr
	r.auditf(id.ID, "config.propose", cr.ID, string(cr.Status), map[string]any{"tier": int(cr.Tier), "changes": len(changes), "title": title})
	return cr, r.save()
}

// statusFor derives where a change request stands from its findings,
// evidence and workflow.
func (r *Repo) statusFor(cr *ChangeRequest) CRStatus {
	// Callers only re-derive the status of requests that are still open.
	if HasErrors(cr.Findings) {
		return CRInvalid
	}
	for _, kind := range RequiredEvidence(cr.Tier) {
		if !slices.ContainsFunc(cr.Evidence, func(e Evidence) bool { return e.Kind == kind }) {
			return CRAwaitingEvidence
		}
	}
	switch cr.Workflow.Status {
	case workflow.Approved:
		return CRApproved
	case workflow.Rejected, workflow.Cancelled:
		return CRRejected
	}
	return CRPendingApproval
}

// AddEvidence attaches a simulation or regression result.
func (r *Repo) AddEvidence(id string, ev Evidence) (*ChangeRequest, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cr, err := r.getLocked(id)
	if err != nil {
		return nil, err
	}
	if cr.Status == CRActive || cr.Status == CRRejected || cr.Status == CRInvalid {
		return nil, fmt.Errorf("change request %s is %s", id, cr.Status)
	}
	if ev.At.IsZero() {
		ev.At = r.now().UTC()
	}
	cr.Evidence = slices.DeleteFunc(cr.Evidence, func(e Evidence) bool { return e.Kind == ev.Kind })
	cr.Evidence = append(cr.Evidence, ev)
	cr.Status = r.statusFor(cr)
	r.auditf("system", "config.evidence", id, ev.Kind, map[string]any{"passed": ev.Passed, "summary": ev.Summary})
	return cr, r.save()
}

// Approve records a person's approval. Approvers are resolved from the
// active configuration and must hold config.approve and the step's role.
func (r *Repo) Approve(id, userID, comment string) (*ChangeRequest, error) {
	return r.decide(id, userID, comment, true)
}

// Reject ends a change request.
func (r *Repo) Reject(id, userID, reason string) (*ChangeRequest, error) {
	return r.decide(id, userID, reason, false)
}

func (r *Repo) decide(id, userID, comment string, approve bool) (*ChangeRequest, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cr, err := r.getLocked(id)
	if err != nil {
		return nil, err
	}
	active, _, err := r.activeLocked()
	if err != nil {
		return nil, err
	}
	actor, err := active.Actor(userID)
	if err != nil {
		return nil, err
	}
	ident, _ := active.Identity(userID)
	if !ident.Can(governance.ActConfigApprove) {
		return nil, fmt.Errorf("%w: %s lacks %s", ErrPermission, userID, governance.ActConfigApprove)
	}
	if cr.Status != CRPendingApproval {
		return nil, fmt.Errorf("change request %s is %s, not pending approval", id, cr.Status)
	}
	if approve {
		err = cr.Workflow.Approve(actor, comment, r.now().UTC())
	} else {
		err = cr.Workflow.Reject(actor, comment, r.now().UTC())
	}
	if err != nil {
		return nil, err
	}
	cr.Status = r.statusFor(cr)
	action := "config.approve"
	if !approve {
		action = "config.reject"
	}
	r.auditf(actor.ID, action, id, string(cr.Status), map[string]any{"comment": comment})
	return cr, r.save()
}

// Activate makes an approved change the new active version.
func (r *Repo) Activate(id, userID string) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cr, err := r.getLocked(id)
	if err != nil {
		return 0, err
	}
	active, version, err := r.activeLocked()
	if err != nil {
		return 0, err
	}
	ident, err := active.Identity(userID)
	if err != nil {
		return 0, err
	}
	if ident.Kind != "human" || !ident.Can(governance.ActConfigActivate) {
		return 0, fmt.Errorf("%w: %s cannot activate configuration", ErrPermission, userID)
	}
	if cr.Status != CRApproved {
		return 0, fmt.Errorf("change request %s is %s, not approved", id, cr.Status)
	}
	if cr.BaseVersion != version {
		return 0, ErrStale
	}
	proposed, err := Parse([]byte(cr.Proposed))
	if err != nil {
		return 0, err
	}
	if fs := Validate(proposed); HasErrors(fs) {
		return 0, fmt.Errorf("change request %s no longer validates", id)
	}
	n, err := r.writeVersion([]byte(cr.Proposed), ident.ID, cr.ID)
	if err != nil {
		return 0, err
	}
	cr.Status, cr.ActivatedVersion = CRActive, n
	r.auditf(ident.ID, "config.activate", cr.ID, fmt.Sprintf("v%d", n), map[string]any{"tier": int(cr.Tier)})
	return n, r.save()
}

// Rollback re-activates the content of an earlier version as a new version.
func (r *Repo) Rollback(to int, userID, reason string) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	active, _, err := r.activeLocked()
	if err != nil {
		return 0, err
	}
	ident, err := active.Identity(userID)
	if err != nil {
		return 0, err
	}
	if ident.Kind != "human" || !ident.Can(governance.ActConfigActivate) {
		return 0, fmt.Errorf("%w: %s cannot roll back configuration", ErrPermission, userID)
	}
	if reason == "" {
		return 0, errors.New("a rollback needs a reason")
	}
	if to < 1 || to >= len(r.state.Versions)+1 || to == r.state.Active {
		return 0, fmt.Errorf("cannot roll back to version %d", to)
	}
	_, doc, err := r.readVersion(to)
	if err != nil {
		return 0, err
	}
	n, err := r.writeVersion(doc, ident.ID, fmt.Sprintf("rollback to v%d", to))
	if err != nil {
		return 0, err
	}
	r.auditf(ident.ID, "config.rollback", fmt.Sprintf("v%d", n), fmt.Sprintf("restored v%d", to), map[string]any{"reason": reason})
	return n, r.save()
}

func (r *Repo) Get(id string) (*ChangeRequest, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.getLocked(id)
}

func (r *Repo) getLocked(id string) (*ChangeRequest, error) {
	cr := r.state.Changes[id]
	if cr == nil {
		return nil, fmt.Errorf("no change request %s", id)
	}
	return cr, nil
}

// ChangeRequests lists change requests, newest first.
func (r *Repo) ChangeRequests() []*ChangeRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*ChangeRequest, 0, len(r.state.Changes))
	for _, cr := range r.state.Changes {
		out = append(out, cr)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out
}

// Versions lists the version history.
func (r *Repo) Versions() []Version {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.state.Versions)
}

func (r *Repo) save() error {
	if err := os.MkdirAll(r.dir, 0o755); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(r.state, "", "  ") // plain data: cannot fail
	return writeAtomic(filepath.Join(r.dir, "state.json"), b)
}

func (r *Repo) auditf(actor, action, subject, outcome string, detail map[string]any) {
	if r.audit == nil {
		return
	}
	if _, err := r.audit.Append(actor, action, subject, outcome, detail); err != nil {
		panic(fmt.Sprintf("audit log write failed: %v", err))
	}
}

// writeAtomic writes via a temporary file and rename, so a crash never
// leaves a half-written file behind.
func writeAtomic(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
