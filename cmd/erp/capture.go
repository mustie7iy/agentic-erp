package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"strings"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/apitools"
)

// requestLimit is the API's request body limit (04-api-design §4.11);
// documents above it go through an upload.
const requestLimit = 5 << 20

// captureCmd captures a supplier invoice from a document file:
//
//	erp capture -legal-entity ID -file invoice.pdf [-content-type application/pdf] [-upload]
//
// A document within the request limit is sent with the capture; a larger
// one (or any, with -upload) goes through a direct upload first: the API
// names where to PUT the bytes (object storage, or a path on the API) and
// the capture then names the upload. It prints the Operation.
func captureCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("capture", flag.ContinueOnError)
	le := fs.String("legal-entity", "", "legal entity receiving the invoice (id)")
	file := fs.String("file", "", "the document: PDF, PNG, JPEG or e-invoice XML")
	ct := fs.String("content-type", "", "content type of the document (default: by extension)")
	force := fs.Bool("upload", false, "upload first even for a small document")
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
	printJSON(op)
	return nil
}

// contentTypeOf is the content type a document's extension implies.
func contentTypeOf(name string) string {
	switch strings.ToLower(name[strings.LastIndex(name, ".")+1:]) {
	case "png":
		return "image/png"
	case "jpg", "jpeg":
		return "image/jpeg"
	case "xml":
		return "application/xml"
	}
	return "application/pdf"
}

// upload creates an upload and PUTs the document where it says.
func upload(ctx context.Context, c *apitools.Client, doc []byte, contentType string) (string, error) {
	var up struct {
		ID      string            `json:"id"`
		URL     string            `json:"url"`
		Method  string            `json:"method"`
		Headers map[string]string `json:"headers"`
	}
	res, err := c.Do(ctx, http.MethodPost, "/v1/uploads", nil, map[string]string{"purpose": "supplier_invoice", "content_type": contentType}, "")
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
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("capture: upload %s: %w", up.ID, err)
	}
	defer func() { _ = resp.Body.Close() }() // only read from: closing frees the connection and cannot change the answer
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("uploading the document: %d %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	fmt.Fprintf(errOut, "uploaded %d bytes as %s\n", len(doc), up.ID)
	return up.ID, nil
}

// capture posts the multipart capture request and returns the Operation.
func capture(ctx context.Context, c *apitools.Client, fields map[string]string, document []byte) (map[string]any, error) {
	var body bytes.Buffer
	// The multipart writer writes to a bytes.Buffer, whose writes never
	// fail, so none of these calls can return an error.
	w := multipart.NewWriter(&body)
	for k, v := range fields {
		_ = w.WriteField(k, v)
	}
	if document != nil {
		part, _ := w.CreateFormFile("document", "document")
		_, _ = part.Write(document)
	}
	_ = w.Close()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.BaseURL, "/")+"/v1/supplier-invoices:capture", &body) // a fixed method and URL always parse
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Idempotency-Key", uuid.NewString())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("capture: send: %w", err)
	}
	defer func() { _ = resp.Body.Close() }() // only read from: closing frees the connection and cannot change the answer
	raw, _ := io.ReadAll(resp.Body)
	var m map[string]any
	if len(raw) > 0 && json.Unmarshal(raw, &m) != nil {
		return nil, fmt.Errorf("the API answered %d with a body that is not JSON", resp.StatusCode)
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%d %v: %v", resp.StatusCode, m["code"], m["detail"])
	}
	return m, nil
}
