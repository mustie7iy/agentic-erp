package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/config"
)

// erp directory is the operators' view across cells: the global control
// plane of api/directory.yaml on erp-router's admin listener
// (ERP_DIRECTORY_API_URL, default http://localhost:9091) with an operator
// token (ERP_OPERATOR_TOKEN).
//
//	erp directory cells [-region eu-west]
//	erp directory tenants [-region R] [-cell C] [-status S] [-order-by "cell asc"] [-page-size N] [-page-token T]
//	erp directory locate -id ID
//	erp directory create -name Acme -region eu-west -currency EUR -admin-subject S -admin-name N [-admin-email E] [-cell C] -config config.yaml
//	erp directory freeze|unfreeze -id ID [-if-match ETAG]
//	erp directory move -id ID -cell C [-if-match ETAG]
func directoryCmd(ctx context.Context, args []string) error {
	if len(args) < 1 {
		return errors.New("usage: erp directory cells|tenants|locate|create|freeze|unfreeze|verify|move [flags]")
	}
	switch args[0] {
	case "cells":
		return directoryCells(ctx, args[1:])
	case "tenants":
		return directoryTenants(ctx, args[1:])
	case "locate":
		return directoryLocate(ctx, args[1:])
	case "create":
		return directoryCreate(ctx, args[1:])
	case "freeze", "unfreeze":
		return directoryTransition(ctx, args[0], args[1:])
	case "verify":
		return directoryVerify(ctx, args[1:])
	case "move":
		return directoryMove(ctx, args[1:])
	}
	return fmt.Errorf("erp directory %s: unknown command (cells, tenants, locate, create, freeze, unfreeze, verify, move)", args[0])
}

// directory performs one call of the global control plane.
func directory(ctx context.Context, method, path string, body []byte, headers map[string]string) (map[string]any, http.Header, error) {
	base := getenv("ERP_DIRECTORY_API_URL")
	if base == "" {
		base = "http://localhost:9091"
	}
	return operatorCall(ctx, base, "/directory/v1"+path, method, body, headers)
}

func directoryCells(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("directory cells", flag.ContinueOnError)
	region := fs.String("region", "", "only cells of this region")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	path := "/cells"
	if *region != "" {
		path += "?region=" + url.QueryEscape(*region)
	}
	cells, _, err := directory(ctx, http.MethodGet, path, nil, nil)
	if err != nil {
		return err
	}
	return printJSON(cells)
}

func directoryTenants(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("directory tenants", flag.ContinueOnError)
	region := fs.String("region", "", "only tenants of this region")
	cell := fs.String("cell", "", "only tenants of this cell")
	status := fs.String("status", "", "only tenants in this status (active, suspended, offboarded, read_only)")
	orderBy := fs.String("order-by", "", `sort order: "updated_at desc" (default) or "cell asc"`)
	size := fs.Int("page-size", 0, "entries per page (default 50, max 500)")
	token := fs.String("page-token", "", "next_page_token of the previous page")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	q := url.Values{}
	for k, v := range map[string]string{"region": *region, "cell": *cell, "status": *status, "order_by": *orderBy, "page_token": *token} {
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
	page, _, err := directory(ctx, http.MethodGet, path, nil, nil)
	if err != nil {
		return err
	}
	return printJSON(page)
}

func directoryLocate(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("directory locate", flag.ContinueOnError)
	id := fs.String("id", "", "tenant id")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *id == "" {
		return errors.New("-id is required")
	}
	e, h, err := directory(ctx, http.MethodGet, "/tenants/"+*id, nil, nil)
	if err != nil {
		return err
	}
	e["etag"] = h.Get("ETag")
	return printJSON(e)
}

func directoryCreate(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("directory create", flag.ContinueOnError)
	name := fs.String("name", "", "tenant name")
	region := fs.String("region", "", "region the tenant's data stays in, e.g. eu-west; decides the cells eligible")
	cell := fs.String("cell", "", "the cell to place the tenant in (default: the emptiest accepting cell of the region)")
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
	in := map[string]any{"name": *name, "region": *region, "group_currency": *currency, "administrator": admin, "configuration": doc}
	if *cell != "" {
		in["cell"] = *cell
	}
	body, _ := json.Marshal(in)
	if *key == "" {
		*key = uuid.NewString()
	}
	created, h, err := directory(ctx, http.MethodPost, "/tenants", body, map[string]string{"Idempotency-Key": *key})
	if err != nil {
		return err
	}
	if h.Get("Idempotent-Replayed") == "true" {
		fmt.Fprintln(errOut, "replayed: this Idempotency-Key already provisioned the tenant")
	}
	return printJSON(created)
}

func directoryTransition(ctx context.Context, verb string, args []string) error {
	fs := flag.NewFlagSet("directory "+verb, flag.ContinueOnError)
	id := fs.String("id", "", "tenant id")
	ifMatch := fs.String("if-match", "", "the entry's ETag (default: read it first)")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *id == "" {
		return errors.New("-id is required")
	}
	if *ifMatch == "" {
		_, h, err := directory(ctx, http.MethodGet, "/tenants/"+*id, nil, nil)
		if err != nil {
			return err
		}
		*ifMatch = h.Get("ETag")
	}
	e, _, err := directory(ctx, http.MethodPost, "/tenants/"+*id+":"+verb, nil, map[string]string{"If-Match": *ifMatch})
	if err != nil {
		return err
	}
	return printJSON(e)
}

// directoryVerify compares the tenant's data in its cell and in another
// (ADR-0030): equal fingerprints are the evidence a move waits for. The
// command exits with an error when they differ, so a script can wait.
func directoryVerify(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("directory verify", flag.ContinueOnError)
	id := fs.String("id", "", "tenant id")
	cell := fs.String("cell", "", "the cell the tenant is being copied to")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *id == "" || *cell == "" {
		return errors.New("-id and -cell are required")
	}
	body, _ := json.Marshal(map[string]string{"cell": strings.TrimSpace(*cell)})
	v, _, err := directory(ctx, http.MethodPost, "/tenants/"+*id+":verify", body, nil)
	if err != nil {
		return err
	}
	if err := printJSON(v); err != nil {
		return err
	}
	if v["equal"] != true {
		return errors.New("the cells differ")
	}
	return nil
}

func directoryMove(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("directory move", flag.ContinueOnError)
	id := fs.String("id", "", "tenant id")
	cell := fs.String("cell", "", "the destination cell (registered, in the tenant's region)")
	ifMatch := fs.String("if-match", "", "the entry's ETag (default: read it first)")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *id == "" || *cell == "" {
		return errors.New("-id and -cell are required")
	}
	if *ifMatch == "" {
		_, h, err := directory(ctx, http.MethodGet, "/tenants/"+*id, nil, nil)
		if err != nil {
			return err
		}
		*ifMatch = h.Get("ETag")
	}
	body, _ := json.Marshal(map[string]string{"cell": strings.TrimSpace(*cell)})
	e, _, err := directory(ctx, http.MethodPost, "/tenants/"+*id+":move", body, map[string]string{"If-Match": *ifMatch})
	if err != nil {
		return err
	}
	return printJSON(e)
}
