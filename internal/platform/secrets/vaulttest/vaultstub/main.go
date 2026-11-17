// Command vaultstub is the vault of the pilot walkthrough
// (scripts/walkthrough.sh), as webhookstub is its webhook subscriber: a
// HashiCorp Vault KV version 2 engine on a loopback port, read with one
// token, that erp-server resolves vault: references against
// (ERP_VAULT_ADDR, ERP_VAULT_TOKEN; ADR-0017). Tenants name their
// connectors' credentials only under vault:<mount>/tenants/<tenant_id>/
// (product owner 2026-11-16), so the walkthrough keeps them here rather
// than in files. A secret is the directory <dir>/<mount>/<path>, each of
// its fields a file of that name holding the value; the directory is read
// on each request, so the walkthrough adds secrets while it runs. The
// token is read from the token file; the walkthrough waits for the ready
// file, which holds the address, as the sign that it listens. It runs
// until it is stopped.
//
//	vaultstub -addr 127.0.0.1:18200 -token token -dir vault -ready vault.url
package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"path"
	"strings"
	"syscall"
	"time"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:18200", "where to listen: a loopback address and port")
	token := flag.String("token", "", "the file holding the token requests must bear")
	dir := flag.String("dir", "", "the directory of the secrets: <mount>/<path>/<field>")
	ready := flag.String("ready", "", "where to write the vault's URL once it listens")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, *addr, *token, *dir, *ready)
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "vaultstub:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, addr, tokenFile, dir, ready string) error {
	ap, err := netip.ParseAddrPort(addr)
	if err != nil || !ap.Addr().IsLoopback() {
		return errors.New("addr: the walkthrough's vault listens on a loopback address and port only")
	}
	token, err := os.ReadFile(tokenFile) // #nosec G304 -- the walkthrough's own file
	if err != nil {
		return fmt.Errorf("token: %w", err)
	}
	root, err := os.OpenRoot(dir) // every read stays below dir, whatever the request's path says
	if err != nil {
		return fmt.Errorf("dir: %w", err)
	}
	defer func() { _ = root.Close() }() // read-only; the process ends next
	ln, err := new(net.ListenConfig).Listen(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	srv := &http.Server{Handler: vault{token: strings.TrimSpace(string(token)), root: root.FS()}, ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	if err := os.WriteFile(ready, []byte("http://"+addr+"\n"), 0o600); err != nil {
		return fmt.Errorf("ready: %w", err)
	}
	select {
	case <-ctx.Done():
		if err := srv.Shutdown(context.Background()); err != nil {
			return fmt.Errorf("stop: %w", err)
		}
		return nil
	case err := <-errc:
		return fmt.Errorf("serve: %w", err)
	}
}

// vault answers KV version 2 reads, as vaulttest.Server does in tests.
type vault struct {
	token string
	root  fs.FS
}

func (v vault) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Vault-Token")), []byte(v.token)) != 1 {
		answer(w, http.StatusForbidden, map[string]any{"errors": []string{"permission denied"}})
		return
	}
	mount, secret, ok := strings.Cut(strings.TrimPrefix(r.URL.Path, "/v1/"), "/data/")
	if !ok || r.Method != http.MethodGet || !fs.ValidPath(path.Join(mount, secret)) {
		answer(w, http.StatusMethodNotAllowed, map[string]any{"errors": []string{"unsupported path"}})
		return
	}
	fields, err := v.read(path.Join(mount, secret))
	if err != nil {
		answer(w, http.StatusNotFound, map[string]any{"errors": []string{}})
		return
	}
	answer(w, http.StatusOK, map[string]any{"data": map[string]any{"data": fields, "metadata": map[string]any{"version": 1}}})
}

// read gives the fields of the secret at dir: its plain files.
func (v vault) read(dir string) (map[string]any, error) {
	entries, err := fs.ReadDir(v.root, dir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	fields := map[string]any{}
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		b, err := fs.ReadFile(v.root, path.Join(dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", e.Name(), err)
		}
		fields[e.Name()] = strings.TrimRight(string(b), "\r\n")
	}
	if len(fields) == 0 {
		return nil, fmt.Errorf("%s: no fields", dir)
	}
	return fields, nil
}

func answer(w http.ResponseWriter, status int, body any) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body) // a failed write means the client is gone: no one is left to tell
}
