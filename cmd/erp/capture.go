package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/apitools"
)

// requestLimit is the API's request body limit (04-api-design §4.11);
// documents above it go through an upload.
const requestLimit = 5 << 20

// uploads PUTs a document where its upload says, object storage's signed
// URL or a path on the API, which takes no bearer token: a client of its
// own, bounded, so a store that stops answering does not hold the command
// for ever (review:batch77).
var uploads = &http.Client{Timeout: 10 * time.Minute}

// captureCmd captures a supplier invoice, or a customer's purchase order
// (-order, T90a), from a document file:
//
//	erp capture -legal-entity ID -file invoice.pdf [-content-type application/pdf] [-upload]
//	erp capture -order -legal-entity ID -file order.txt -account ID [-tax-code CODE]
//
// A document within the request limit is sent with the capture; a larger
// one (or any, with -upload) goes through a direct upload first: the API
// names where to PUT the bytes (object storage, or a path on the API) and
// the capture then names the upload. An order always goes through an
// upload, and its items take the revenue account and tax code given. It
// prints the Operation.
func captureCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("capture", flag.ContinueOnError)
	le := fs.String("legal-entity", "", "legal entity receiving the invoice or taking the order (id)")
	file := fs.String("file", "", "the document: PDF, PNG, JPEG or e-invoice XML; an order PDF or plain text")
	ct := fs.String("content-type", "", "content type of the document (default: by extension)")
	force := fs.Bool("upload", false, "upload first even for a small document")
	order := fs.Bool("order", false, "capture a customer's purchase order into a draft sales order")
	account := fs.String("account", "", "the order: revenue account of every item (id)")
	taxCode := fs.String("tax-code", "", "the order: output tax code of every item")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *le == "" || *file == "" {
		return errors.New("-legal-entity and -file are required")
	}
	if _, err := uuid.Parse(*le); err != nil {
		return fmt.Errorf("-legal-entity %q is not an id", *le)
	}
	doc, err := os.ReadFile(*file)
	if err != nil {
		return fmt.Errorf("capture: read document: %w", err)
	}
	if *ct == "" {
		*ct = contentTypeOf(*file)
	}
	if *order {
		return captureOrder(ctx, map[string]string{"legal_entity_id": *le, "account_id": *account, "tax_code": *taxCode}, doc, *ct)
	}
	c, err := client("ERP_TOKEN")
	if err != nil {
		return err
	}
	fields := map[string]string{"legal_entity_id": *le}
	var document []byte
	if *force || len(doc) > requestLimit {
		id, err := upload(ctx, c, doc, *ct)
		if err != nil {
			return err
		}
		fields["upload_id"] = id
	} else {
		document = doc
	}
	op, err := capture(ctx, c, fields, document)
	if err != nil {
		return err
	}
	return printJSON(op)
}

// captureOrder uploads a customer's purchase order and captures it into a
// draft sales order (POST /v1/sales-orders:capture); fields are the
// company, the account and the tax code, "" for none.
func captureOrder(ctx context.Context, fields map[string]string, doc []byte, contentType string) error {
	if _, err := uuid.Parse(fields["account_id"]); err != nil {
		return fmt.Errorf("-account %q is not an id; an order's items need their revenue account", fields["account_id"])
	}
	c, err := client("ERP_TOKEN")
	if err != nil {
		return err
	}
	op, err := orderCapture(ctx, c, fields, doc, contentType)
	if err != nil {
		return err
	}
	return printJSON(op)
}

// orderCapture uploads the document as a customer's purchase order and
// captures it, returning the Operation.
func orderCapture(ctx context.Context, c *apitools.Client, fields map[string]string, doc []byte, contentType string) (map[string]any, error) {
	id, err := uploadFor(ctx, c, doc, contentType, "sales_order")
	if err != nil {
		return nil, err
	}
	body := map[string]string{"legal_entity_id": fields["legal_entity_id"], "upload_id": id, "account_id": fields["account_id"]}
	if fields["tax_code"] != "" {
		body["tax_code"] = fields["tax_code"]
	}
	res, err := c.Do(ctx, http.MethodPost, "/v1/sales-orders:capture", nil, body, "")
	if err != nil {
		return nil, err
	}
	var op map[string]any
	if err := res.Decode(&op); err != nil {
		return nil, err
	}
	return op, nil
}

// contentTypeOf is the content type a document's extension implies.
func contentTypeOf(name string) string {
	switch strings.ToLower(name[strings.LastIndex(name, ".")+1:]) {
	case "txt":
		return "text/plain"
	case "png":
		return "image/png"
	case "jpg", "jpeg":
		return "image/jpeg"
	case "xml":
		return "application/xml"
	}
	return "application/pdf"
}

// upload creates an upload of a supplier invoice and PUTs the document
// where it says.
func upload(ctx context.Context, c *apitools.Client, doc []byte, contentType string) (string, error) {
	return uploadFor(ctx, c, doc, contentType, "supplier_invoice")
}

// uploadFor creates an upload for the purpose and PUTs the document where
// it says.
func uploadFor(ctx context.Context, c *apitools.Client, doc []byte, contentType, purpose string) (string, error) {
	var up struct {
		ID      string            `json:"id"`
		URL     string            `json:"url"`
		Method  string            `json:"method"`
		Headers map[string]string `json:"headers"`
	}
	// An upload's permission follows its purpose: a customer's purchase
	// order has an operation of its own (T90a).
	path, body := "/v1/uploads", map[string]string{"purpose": purpose, "content_type": contentType}
	if purpose == "sales_order" {
		path, body = "/v1/sales-order-uploads", map[string]string{"content_type": contentType}
	}
	res, err := c.Do(ctx, http.MethodPost, path, nil, body, "")
	if err != nil {
		return "", err
	}
	if err := res.Decode(&up); err != nil {
		return "", err
	}
	target := up.URL
	if strings.HasPrefix(target, "/") { // a path on the API
		target = strings.TrimRight(c.BaseURL, "/") + target
	}
	req, err := http.NewRequestWithContext(ctx, up.Method, target, bytes.NewReader(doc))
	if err != nil {
		return "", fmt.Errorf("upload URL: %w", err)
	}
	for k, v := range up.Headers {
		req.Header.Set(k, v)
	}
	resp, err := uploads.Do(req)
	if err != nil {
		return "", fmt.Errorf("capture: upload %s: %w", up.ID, err)
	}
	defer func() { _ = resp.Body.Close() }() // only read from: closing frees the connection and cannot change the answer
	if resp.StatusCode >= 300 {
		// The refusal's status is the error; its body only explains it, so
		// a body cut off still reports the status with what arrived.
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("uploading the document: %d %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	fmt.Fprintf(errOut, "uploaded %d bytes as %s\n", len(doc), up.ID)
	return up.ID, nil
}

// capture posts the multipart capture request and returns the Operation
// (apitools.Client.Form: the form, its Idempotency-Key and the problem of
// a refusal are the client's).
func capture(ctx context.Context, c *apitools.Client, fields map[string]string, document []byte) (map[string]any, error) {
	form := maps.Clone(fields)
	if document != nil {
		// A value, not a file part: the contract's document is a string of
		// bytes (contentMediaType), which the server reads from the form's
		// values.
		form["document"] = string(document)
	}
	res, err := c.Form(ctx, "/v1/supplier-invoices:capture", form)
	if err != nil {
		return nil, fmt.Errorf("capture: send: %w", err)
	}
	var op map[string]any
	if err := res.Decode(&op); err != nil {
		return nil, fmt.Errorf("capture: %w", err)
	}
	return op, nil
}
