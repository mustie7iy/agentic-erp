package controlplane

import (
	"context"
	"io"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/agents"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/httpapi"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// AIPolicy is the AiPolicy schema of the contract: the tenant's AI kill
// switch, both hands of it (ADR-0036). Routes, models and caps are the
// tenant's own business and stay out.
type AIPolicy struct {
	TenantID        uuid.UUID `json:"tenant_id"`
	Enabled         bool      `json:"enabled"`
	Suspended       bool      `json:"suspended"`
	SuspendedReason *string   `json:"suspended_reason,omitempty"`
	Active          bool      `json:"active"`
	Version         int32     `json:"version"`
}

func aiPolicyView(tenant uuid.UUID, p agents.Policy) AIPolicy {
	return AIPolicy{TenantID: tenant, Enabled: p.Enabled, Suspended: p.Suspended, SuspendedReason: p.SuspendedReason, Active: p.Active(), Version: p.Version}
}

// AIPolicy reads the tenant's AI kill switch.
func (s *Service) AIPolicy(ctx context.Context, tenant uuid.UUID) (AIPolicy, error) {
	var out AIPolicy
	err := s.inTenant(ctx, tenant, nil, func(ctx context.Context, tx pgx.Tx) error {
		p, err := agents.ReadPolicy(ctx, tx)
		out = aiPolicyView(tenant, p)
		return err
	})
	return out, err
}

// SuspendAI switches the tenant's AI off on the operators' hand, guarded
// by If-Match. The reason stays out of the event and the audit entry: it is
// the operator's words.
func (s *Service) SuspendAI(ctx context.Context, op Operator, tenant uuid.UUID, ifMatch, reason string) (AIPolicy, error) {
	return s.switchAI(ctx, op, tenant, "suspend", func(ctx context.Context, tx pgx.Tx) (agents.Policy, error) {
		return agents.Suspend(ctx, tx, op.ID, etag.IfMatch(ifMatch), reason)
	})
}

// ResumeAI lifts the operators' suspension, guarded by If-Match.
func (s *Service) ResumeAI(ctx context.Context, op Operator, tenant uuid.UUID, ifMatch string) (AIPolicy, error) {
	return s.switchAI(ctx, op, tenant, "resume", func(ctx context.Context, tx pgx.Tx) (agents.Policy, error) {
		return agents.Resume(ctx, tx, op.ID, etag.IfMatch(ifMatch))
	})
}

func (s *Service) switchAI(ctx context.Context, op Operator, tenant uuid.UUID, verb string, apply func(ctx context.Context, tx pgx.Tx) (agents.Policy, error)) (AIPolicy, error) {
	var out AIPolicy
	err := s.inTenant(ctx, tenant, &op, func(ctx context.Context, tx pgx.Tx) error {
		p, err := apply(ctx, tx)
		if err != nil {
			return err
		}
		out = aiPolicyView(tenant, p)
		event, err := agents.PolicyEvent(ctx, tx, tenant, p, []string{"suspended"})
		if err != nil {
			return err
		}
		entry := audit.Entry{Action: "agents.ai_policy." + verb, ObjectType: "agents.ai_policy", ObjectID: tenant,
			Detail: map[string]any{"active": p.Active(), "version": p.Version}}
		return s.record(ctx, tx, op, tenant, change{event: &event, entry: entry})
	})
	return out, err
}

// aiPolicy serves {id}/ai-policy and its :suspend and :resume.
func (h *handler) aiPolicy(w http.ResponseWriter, r *http.Request) {
	_, verb, _ := strings.Cut(r.URL.Path, ":")
	switch {
	case verb == "" && r.Method == http.MethodGet:
		h.serveIn(w, r, "getTenantAiPolicy", func(w http.ResponseWriter, r *http.Request, tenant uuid.UUID) error {
			return aiPolicyAnswer(w)(h.s.AIPolicy(r.Context(), tenant))
		})
	case verb == "suspend" && r.Method == http.MethodPost:
		h.serveIn(w, r, "suspendAi", h.suspendAI)
	case verb == "resume" && r.Method == http.MethodPost:
		h.serveIn(w, r, "resumeAi", func(w http.ResponseWriter, r *http.Request, tenant uuid.UUID) error {
			return aiPolicyAnswer(w)(h.s.ResumeAI(r.Context(), operator(r.Context()), tenant, r.Header.Get("If-Match")))
		})
	case verb == "":
		httpapi.NotAllowed(w, r, "GET")
	default:
		httpapi.NotAllowed(w, r, "POST")
	}
}

func (h *handler) suspendAI(w http.ResponseWriter, r *http.Request, tenant uuid.UUID) error {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4096))
	if err != nil {
		return problem.New(http.StatusRequestEntityTooLarge, "request.body_too_large", "Request body too large", "The request body exceeds 4 KB.").WithCause(err)
	}
	var in struct {
		Reason string `json:"reason"`
	}
	if err := strictJSON(body, &in); err != nil {
		return err
	}
	return aiPolicyAnswer(w)(h.s.SuspendAI(r.Context(), operator(r.Context()), tenant, r.Header.Get("If-Match"), in.Reason))
}

func aiPolicyAnswer(w http.ResponseWriter) func(AIPolicy, error) error {
	return func(p AIPolicy, err error) error {
		if err != nil {
			return err
		}
		w.Header().Set("ETag", etag.Format(p.Version))
		httpapi.WriteJSON(w, http.StatusOK, p)
		return nil
	}
}
