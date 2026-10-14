package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/config"
)

// erp tenants is the operators' side: the control plane of api/control.yaml
// on a server's admin listener (ERP_CONTROL_URL, default
// http://localhost:9090) with an operator token (ERP_OPERATOR_TOKEN; erp
// token -subject NAME without -tenant mints one from a development issuer).
//
//	erp tenants list [-status active] [-order-by "name asc"] [-page-size N] [-page-token T]
//	erp tenants get -id ID
//	erp tenants create -name Acme -region eu-west -currency EUR -admin-subject S -admin-name N [-admin-email E] -config config.yaml
//	erp tenants suspend|resume|offboard|rotate-key|erase -id ID [-if-match ETAG]
//
// Each command prints the answer as JSON; a transition without -if-match
// reads the tenant first and uses its ETag.
func tenantsCmd(ctx context.Context, args []string) error {
	if len(args) < 1 {
		return errors.New("usage: erp tenants list|get|fingerprint|create|suspend|resume|offboard|rotate-key|erase [flags]")
	}
	switch args[0] {
	case "list":
		return tenantsList(ctx, args[1:])
	case "get":
		return tenantsGet(ctx, args[1:])
	case "fingerprint":
		return tenantsFingerprint(ctx, args[1:])
	case "create":
		return tenantsCreate(ctx, args[1:])
	case "suspend", "resume", "offboard", "rotate-key", "erase":
		return tenantsTransition(ctx, args[0], args[1:])
	}
	return fmt.Errorf("erp tenants %s: unknown command (list, get, fingerprint, create, suspend, resume, offboard, rotate-key, erase)", args[0])
}

// tenantsFingerprint prints the tenant with its fingerprint: every table's
// rows and the audit chain head (ADR-0030), what a move compares.
func tenantsFingerprint(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("tenants fingerprint", flag.ContinueOnError)
	id := fs.String("id", "", "tenant id")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *id == "" {
		return errors.New("-id is required")
	}
	t, h, err := control(ctx, http.MethodGet, "/tenants/"+*id+"/fingerprint", nil, nil)
	if err != nil {
		return err
	}
	t["etag"] = h.Get("ETag")
	return printJSON(t)
}

// control performs one call of the control plane and returns the body and
// the headers; a problem answer is an error with its detail.
func control(ctx context.Context, method, path string, body []byte, headers map[string]string) (map[string]any, http.Header, error) {
	base := getenv("ERP_CONTROL_URL")
	if base == "" {
		base = "http://localhost:9090"
	}
	return operatorCall(ctx, base, "/control/v1"+path, method, body, headers)
}

// operatorCall performs one call as the operator (ERP_OPERATOR_TOKEN) on a
// control plane: the cell's or the global one.
func operatorCall(ctx context.Context, base, path, method string, body []byte, headers map[string]string) (map[string]any, http.Header, error) {
	token := getenv("ERP_OPERATOR_TOKEN")
	if token == "" {
		return nil, nil, errors.New("ERP_OPERATOR_TOKEN is not set (erp token -subject NAME mints one from a development issuer)")
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(base, "/")+path, bytes.NewReader(body)) // #nosec G704 -- the operator's own control plane, from settings
	if err != nil {
		return nil, nil, fmt.Errorf("control plane URL %q: %w", base, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req) // #nosec G704 -- see above
	if err != nil {
		return nil, nil, fmt.Errorf("control plane %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }() // only read from: closing frees the connection and cannot change the answer
	raw, _ := io.ReadAll(resp.Body)
	var m map[string]any
	if len(raw) > 0 && json.Unmarshal(raw, &m) != nil {
		return nil, nil, fmt.Errorf("the control plane answered %d with a body that is not JSON: is the URL its admin listener?", resp.StatusCode)
	}
	if resp.StatusCode >= 400 {
		return nil, nil, fmt.Errorf("%d %v: %v", resp.StatusCode, m["code"], m["detail"])
	}
	return m, resp.Header, nil
}

// printJSON prints an answer as indented JSON; the error is a failed write.
func printJSON(v any) error {
	b, _ := json.MarshalIndent(v, "", "  ") // decoded JSON always encodes
	return printf("answer", "%s\n", b)
}

func tenantsList(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("tenants list", flag.ContinueOnError)
	status := fs.String("status", "", "only tenants in this status (provisioning, active, suspended, offboarded)")
	orderBy := fs.String("order-by", "", `sort order: "created_at desc" (default) or "name asc"`)
	size := fs.Int("page-size", 0, "tenants per page (default 50, max 500)")
	token := fs.String("page-token", "", "next_page_token of the previous page")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	q := url.Values{}
	for k, v := range map[string]string{"status": *status, "order_by": *orderBy, "page_token": *token} {
		if v != "" {
			q.Set(k, v)
		}
	}
	if *size > 0 {
		q.Set("page_size", fmt.Sprint(*size))
	}
	path := "/tenants"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	page, _, err := control(ctx, http.MethodGet, path, nil, nil)
	if err != nil {
		return err
	}
	return printJSON(page)
}

func tenantsGet(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("tenants get", flag.ContinueOnError)
	id := fs.String("id", "", "tenant id")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *id == "" {
		return errors.New("-id is required")
	}
	t, h, err := control(ctx, http.MethodGet, "/tenants/"+*id, nil, nil)
	if err != nil {
		return err
	}
	t["etag"] = h.Get("ETag")
	return printJSON(t)
}

func tenantsCreate(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("tenants create", flag.ContinueOnError)
	name := fs.String("name", "", "tenant name")
	region := fs.String("region", "", "region the tenant's data stays in, e.g. eu-west")
	currency := fs.String("currency", "", "group currency (ISO 4217)")
	subject := fs.String("admin-subject", "", "the administrator's subject at the identity provider")
	adminName := fs.String("admin-name", "", "the administrator's display name")
	email := fs.String("admin-email", "", "the administrator's email (optional)")
	file := fs.String("config", "", "the first configuration (YAML; erp config generate writes one)")
	key := fs.String("idempotency-key", "", "Idempotency-Key of the request (default: a new UUID; reuse it to retry safely)")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *file == "" {
		return errors.New("-config is required: the tenant's first configuration")
	}
	doc, err := config.Load(*file)
	if err != nil {
		return err
	}
	admin := map[string]any{"subject": *subject, "display_name": *adminName}
	if *email != "" {
		admin["email"] = *email
	}
	body, _ := json.Marshal(map[string]any{"name": *name, "region": *region, "group_currency": *currency, "administrator": admin, "configuration": doc})
	if *key == "" {
		*key = uuid.NewString()
	}
	created, h, err := control(ctx, http.MethodPost, "/tenants", body, map[string]string{"Idempotency-Key": *key})
	if err != nil {
		return err
	}
	if h.Get("Idempotent-Replayed") == "true" {
		fmt.Fprintln(errOut, "replayed: this Idempotency-Key already provisioned the tenant")
	}
	return printJSON(created)
}

func tenantsTransition(ctx context.Context, verb string, args []string) error {
	fs := flag.NewFlagSet("tenants "+verb, flag.ContinueOnError)
	id := fs.String("id", "", "tenant id")
	ifMatch := fs.String("if-match", "", "the tenant's ETag (default: read it first)")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *id == "" {
		return errors.New("-id is required")
	}
	if *ifMatch == "" {
		_, h, err := control(ctx, http.MethodGet, "/tenants/"+*id, nil, nil)
		if err != nil {
			return err
		}
		*ifMatch = h.Get("ETag")
	}
	method := verb
	if verb == "rotate-key" {
		method = "rotateKey" // custom methods are lowerCamelCase in the contract
	}
	t, _, err := control(ctx, http.MethodPost, "/tenants/"+*id+":"+method, nil, map[string]string{"If-Match": *ifMatch})
	if err != nil {
		return err
	}
	return printJSON(t)
}
