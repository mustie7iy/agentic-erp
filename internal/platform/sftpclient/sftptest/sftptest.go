// Package sftptest is a test double of a bank's SFTP server: an in-process
// server on an in-memory file system that accepts one client key and can be
// told to fail a step, so the SFTP transport of ADR-0038 and the payments
// that use it are tested without a server (06-engineering-process §6.6).
package sftptest

import (
	"bufio"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"testing"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/mustie7iy/agentic-erp/internal/platform/sftpclient"
)

// Server is the bank's SFTP server. Set its fields in the configure
// function NewServer takes; they are read while it serves.
type Server struct {
	Addr        string
	HostSigners []ssh.Signer  // the first is the key a client pins
	Accepted    chan struct{} // a Silent server signals each connection it takes
	Silent      bool          // reads the client's version line and never answers
	NoSubsystem bool
	FailOpen    bool
	FailWrite   bool
	FailRename  bool

	listener  net.Listener
	clientKey ssh.PublicKey
	files     sftp.Handlers
}

// Signer is a new ed25519 key.
func Signer(tb testing.TB) ssh.Signer {
	tb.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		tb.Fatal(err)
	}
	return SignerOf(tb, key)
}

// SignerOf is a signer of a private key.
func SignerOf(tb testing.TB, key any) ssh.Signer {
	tb.Helper()
	s, err := ssh.NewSignerFromKey(key)
	if err != nil {
		tb.Fatal(err)
	}
	return s
}

// PrivateKey is a new ed25519 private key in OpenSSH PEM form, as a secret
// holds it, and its signer.
func PrivateKey(tb testing.TB) ([]byte, ssh.Signer) {
	tb.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		tb.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(key, "")
	if err != nil {
		tb.Fatal(err)
	}
	return pem.EncodeToMemory(block), SignerOf(tb, key)
}

// NewServer starts the server for a client key; it stops with the test.
func NewServer(tb testing.TB, client ssh.PublicKey, configure func(*Server)) *Server {
	tb.Helper()
	l, err := (&net.ListenConfig{}).Listen(tb.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = l.Close() }) // closing stops serve
	s := &Server{Addr: l.Addr().String(), listener: l, HostSigners: []ssh.Signer{Signer(tb)}, clientKey: client, files: sftp.InMemHandler(),
		Accepted: make(chan struct{}, 1)}
	configure(s)
	go s.serve()
	return s
}

// HostKey is the server's pinned key in authorized_keys form.
func (s *Server) HostKey() string {
	return string(ssh.MarshalAuthorizedKey(s.HostSigners[0].PublicKey()))
}

// Endpoint is the server's root directory, signed in to as erp.
func (s *Server) Endpoint(client ssh.Signer) sftpclient.Endpoint {
	return sftpclient.Endpoint{Addr: s.Addr, User: "erp", HostKey: s.HostSigners[0].PublicKey(), Signer: client, Dir: "/"}
}

func (s *Server) serve() {
	cfg := &ssh.ServerConfig{PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		if string(key.Marshal()) != string(s.clientKey.Marshal()) {
			return nil, errors.New("unknown key")
		}
		return &ssh.Permissions{}, nil
	}}
	for _, h := range s.HostSigners {
		cfg.AddHostKey(h)
	}
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		if s.Silent {
			go func() {
				_, _ = bufio.NewReader(conn).ReadString('\n') // the client's version line: it now waits for the server's
				s.Accepted <- struct{}{}
			}()
			continue
		}
		go s.session(conn, cfg)
	}
}

func (s *Server) session(conn net.Conn, cfg *ssh.ServerConfig) {
	_, chans, reqs, err := ssh.NewServerConn(conn, cfg)
	if err != nil {
		return
	}
	go ssh.DiscardRequests(reqs)
	for nc := range chans {
		ch, requests, err := nc.Accept()
		if err != nil {
			return
		}
		go func() {
			for r := range requests {
				ok := r.Type == "subsystem" && !s.NoSubsystem
				_ = r.Reply(ok, nil) // a client gone before the reply ends the session anyway
				if ok {
					go func() { _ = sftp.NewRequestServer(ch, s.handlers()).Serve(); _ = ch.Close() }() // as a real server ends the session
				}
			}
		}()
	}
}

// handlers are the in-memory file system with the failures asked for.
func (s *Server) handlers() sftp.Handlers {
	h := s.files
	h.FilePut, h.FileCmd = failing{s, h.FilePut, h.FileCmd}, failing{s, h.FilePut, h.FileCmd}
	return h
}

type failing struct {
	s   *Server
	put sftp.FileWriter
	cmd sftp.FileCmder
}

func (f failing) Filewrite(r *sftp.Request) (io.WriterAt, error) {
	if f.s.FailOpen {
		return nil, errors.New("disk full")
	}
	w, err := f.put.Filewrite(r)
	if err != nil {
		return nil, fmt.Errorf("sftptest: open %s: %w", r.Filepath, err)
	}
	if f.s.FailWrite {
		return badWriter{}, nil
	}
	return w, nil
}

func (f failing) Filecmd(r *sftp.Request) error {
	if f.s.FailRename && r.Method == "Rename" {
		return errors.New("rename refused")
	}
	if err := f.cmd.Filecmd(r); err != nil {
		return fmt.Errorf("sftptest: %s %s: %w", r.Method, r.Filepath, err)
	}
	return nil
}

type badWriter struct{}

func (badWriter) WriteAt([]byte, int64) (int, error) { return 0, errors.New("quota exceeded") }

// Read is a file of the server's file system, up to 64 KiB.
func (s *Server) Read(tb testing.TB, path string) (string, bool) {
	tb.Helper()
	r := sftp.NewRequest("Get", path)
	r.Flags = 1 // SSH_FXF_READ
	ra, err := s.files.FileGet.Fileread(r)
	if err != nil {
		return "", false
	}
	buf := make([]byte, 64<<10)
	n, _ := ra.ReadAt(buf, 0) // io.EOF after a short file
	return string(buf[:n]), true
}
