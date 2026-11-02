// Command modelstub stands in for the model provider in the pilot
// walkthrough (scripts/walkthrough.sh), as the mock SAP extract stands in
// for SAP: it answers every Messages call (POST /v1/messages) with the text
// of one file, which the walkthrough writes as the document it uploads
// prints, so a capture's extraction is known without a live model or its
// key. It reads nothing of the request but its model, which it echoes.
//
//	modelstub -addr 127.0.0.1:18081 -answer extraction.json
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:18081", "where to listen")
	answer := flag.String("answer", "", "the file whose text every message answers")
	flag.Parse()
	srv := &http.Server{Addr: *addr, Handler: handler(*answer), ReadHeaderTimeout: 10 * time.Second}
	if err := srv.ListenAndServe(); err != nil {
		fmt.Fprintln(os.Stderr, "modelstub:", err)
		os.Exit(1)
	}
}

func handler(answer string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/messages", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 64<<20)).Decode(&req); err != nil {
			http.Error(w, "modelstub: the request is not a message: "+err.Error(), http.StatusBadRequest)
			return
		}
		text, err := os.ReadFile(answer) // #nosec G304 -- the walkthrough names its own file
		if err != nil {
			http.Error(w, "modelstub: "+err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{ // the gateway reports a broken answer as the provider failing
			"id": "msg_walkthrough", "type": "message", "role": "assistant", "model": req.Model,
			"content":     []map[string]any{{"type": "text", "text": string(text)}},
			"stop_reason": "end_turn", "stop_sequence": nil,
			"usage": map[string]any{"input_tokens": 1, "output_tokens": 1},
		})
	})
	return mux
}
