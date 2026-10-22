package httpapi

import (
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"

	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// fileParts reads a multipart/form-data body into the form's values, file
// parts included, before the generated server decodes it. The contract
// types a file in a multipart body as a string of bytes (contentMediaType
// application/octet-stream, OpenAPI 3.1), which clients send as a file
// part (curl -F file=@statement.xml, a browser's file input); the
// generated server reads such fields from the form's values alone, so it
// refused the file part with 400 request.invalid_body and accepted only a
// plain field (walkthrough, 2026-10-22). No field of the contract is a
// multipart file of the generated server's own (format: binary), so every
// part becomes a value. The unit of work has read and bounded the body
// already (uow.MaxBody), so holding the parts in memory adds no new limit.
func fileParts(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); ct != "multipart/form-data" { // a malformed type is the generated server's to refuse
			next.ServeHTTP(w, r)
			return
		}
		form, err := readForm(r)
		if err != nil {
			problem.Write(w, r, problem.BadRequest("request.invalid_body", "The request body is not a readable multipart form.").WithCause(err))
			return
		}
		r.MultipartForm = form
		next.ServeHTTP(w, r)
	})
}

// readForm reads every part of the body as a value under its form name,
// in the order of the parts.
func readForm(r *http.Request) (*multipart.Form, error) {
	mr, err := r.MultipartReader()
	if err != nil {
		return nil, fmt.Errorf("httpapi: multipart body: %w", err)
	}
	form := &multipart.Form{Value: map[string][]string{}, File: map[string][]*multipart.FileHeader{}}
	for {
		p, err := mr.NextPart()
		if err == io.EOF { //nolint:errorlint // NextPart returns io.EOF itself after the last part; a body cut short is an error wrapping it
			return form, nil
		}
		if err != nil {
			return nil, fmt.Errorf("httpapi: multipart body: %w", err)
		}
		b, err := io.ReadAll(p)
		if err != nil {
			return nil, fmt.Errorf("httpapi: multipart part %q: %w", p.FormName(), err)
		}
		form.Value[p.FormName()] = append(form.Value[p.FormName()], string(b))
	}
}
