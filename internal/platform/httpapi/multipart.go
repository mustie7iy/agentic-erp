package httpapi

import (
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"

	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/uow"
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
// part becomes a value (api's TestNoBinaryMultipartFields keeps it so).
// Only the bodies of unsafe methods (uow.Unsafe) are read: no operation
// reads another's. They are bounded as the unit of work bounds them
// (uow.MaxBody) and to maxParts parts.
func fileParts(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")) // a malformed type is the generated server's to refuse
		if ct != "multipart/form-data" || !uow.Unsafe(r.Method) {
			next.ServeHTTP(w, r)
			return
		}
		form, err := readForm(w, r)
		if err != nil {
			problem.Write(w, r, problem.BadRequest("request.invalid_body", "The request body is not a readable multipart form.").WithCause(err))
			return
		}
		r.MultipartForm = form
		next.ServeHTTP(w, r)
	})
}

// maxParts bounds a form's parts, as multipart.Reader.ReadForm does by
// default: a body of many tiny parts would otherwise grow into a large map.
const maxParts = 1000

// readForm reads every part of the body as a value under its form name,
// in the order of the parts, from at most uow.MaxBody bytes and maxParts
// parts, so it does not depend on the unit of work having bounded it.
func readForm(w http.ResponseWriter, r *http.Request) (*multipart.Form, error) {
	r.Body = http.MaxBytesReader(w, r.Body, uow.MaxBody)
	mr, err := r.MultipartReader()
	if err != nil {
		return nil, fmt.Errorf("httpapi: multipart body: %w", err)
	}
	form := &multipart.Form{Value: map[string][]string{}, File: map[string][]*multipart.FileHeader{}}
	for n := 0; ; n++ {
		p, err := mr.NextPart()
		if err == io.EOF { //nolint:errorlint // NextPart returns io.EOF itself after the last part; a body cut short is an error wrapping it
			return form, nil
		}
		if err != nil {
			return nil, fmt.Errorf("httpapi: multipart body: %w", err)
		}
		if n == maxParts {
			return nil, fmt.Errorf("httpapi: multipart body: more than %d parts", maxParts)
		}
		b, err := io.ReadAll(p)
		if err != nil {
			return nil, fmt.Errorf("httpapi: multipart part %q: %w", p.FormName(), err)
		}
		form.Value[p.FormName()] = append(form.Value[p.FormName()], string(b))
	}
}
