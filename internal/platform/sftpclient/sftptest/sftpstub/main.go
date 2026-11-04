// Command sftpstub stands in for a treasury system's SFTP server in the
// pilot walkthrough (scripts/walkthrough.sh), as modelstub stands in for
// the model provider: the test double of ADR-0038's transport
// (sftptest.Server) on a port of its own, its files in memory. It signs in
// one client key, pins a host key it makes at start and writes in
// authorized_keys form (the walkthrough configures the connector with it,
// and waits for the file as the sign that the server listens), makes the
// directories asked for, and keeps a copy of every file renamed into place
// in the out directory, so the walkthrough reads what the treasury system
// received. It runs until it is stopped.
//
//	sftpstub -addr 127.0.0.1:18022 -client-key client.pub -host-key host.pub -mkdir /treasury -out received
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"syscall"

	"golang.org/x/crypto/ssh"

	"github.com/mustie7iy/agentic-erp/internal/platform/sftpclient/sftptest"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:18022", "where to listen")
	clientKey := flag.String("client-key", "", "the client's public key, in authorized_keys form")
	hostKey := flag.String("host-key", "", "where to write the server's public key, in authorized_keys form")
	dir := flag.String("mkdir", "/", "a directory to make on the server")
	out := flag.String("out", "", "the directory every received file is copied to")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, *addr, *clientKey, *hostKey, *dir, *out)
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "sftpstub:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, addr, clientKey, hostKey, dir, out string) error {
	line, err := os.ReadFile(clientKey) // #nosec G304 -- the walkthrough names its own file
	if err != nil {
		return fmt.Errorf("client key: %w", err)
	}
	client, _, _, _, err := ssh.ParseAuthorizedKey(line)
	if err != nil {
		return fmt.Errorf("client key: %w", err)
	}
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return fmt.Errorf("host key: %w", err)
	}
	host, err := ssh.NewSignerFromKey(key)
	if err != nil {
		return fmt.Errorf("host key: %w", err)
	}
	s, err := sftptest.Start(ctx, addr, client, host, func(s *sftptest.Server) {
		s.Stored = func(p string) {
			if err := keep(s, p, out); err != nil {
				fmt.Fprintln(os.Stderr, "sftpstub:", err)
			}
		}
	})
	if err != nil {
		return err
	}
	defer func() { _ = s.Close() }() // stopping: nothing is left to serve
	if dir != "/" {
		if err := s.MakeDir(dir); err != nil {
			return err
		}
	}
	if err := os.WriteFile(hostKey, []byte(s.HostKey()), 0o600); err != nil { // #nosec G703 -- the walkthrough names its own file
		return fmt.Errorf("host key: %w", err)
	}
	fmt.Println("sftpstub: listening on", s.Addr)
	<-ctx.Done()
	return nil
}

// keep copies the file received at p into out, under its name.
func keep(s *sftptest.Server, p, out string) error {
	b, err := s.ReadFile(p, 64<<20)
	if err != nil {
		return err
	}
	if path.Base(p) == "." || path.Base(p) == "/" {
		return errors.New("a file without a name: " + p)
	}
	fmt.Printf("sftpstub: received %s (%d bytes)\n", p, len(b))
	if err := os.WriteFile(filepath.Join(out, path.Base(p)), b, 0o600); err != nil { // #nosec G304 G703 -- the walkthrough's own directory, a received file's base name
		return fmt.Errorf("keep %s: %w", p, err)
	}
	return nil
}
