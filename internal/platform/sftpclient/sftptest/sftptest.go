// Package sftptest is a test double of a bank's SFTP server: an in-process
// server on an in-memory file system that accepts one client key and can be
// told to fail a step, so the SFTP transport of ADR-0038 and the payments
// that use it are tested without a server (06-engineering-process §6.6).
package sftptest

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
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
	FailRemove  bool
	FailList    bool // listing a directory
	FailRead    bool // reading a file's content
	// Stored, when set, is told the path of each file renamed into place:
	// the walkthrough's stand-in keeps a copy (sftpstub).
	Stored func(path string)

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
	s, err := Start(tb.Context(), "127.0.0.1:0", client, Signer(tb), configure)
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = s.Close() }) // closing stops serve
	return s
}

// Start starts the server on addr for a client key, with host as its
// pinned key, until Close: the walkthrough's stand-in runs it outside a
// test (sftpstub).
func Start(ctx context.Context, addr string, client ssh.PublicKey, host ssh.Signer, configure func(*Server)) (*Server, error) {
	l, err := (&net.ListenConfig{}).Listen(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("sftptest: listen on %s: %w", addr, err)
	}
	s := &Server{Addr: l.Addr().String(), listener: l, HostSigners: []ssh.Signer{host}, clientKey: client, files: sftp.InMemHandler(),
		Accepted: make(chan struct{}, 1)}
	configure(s)
	go s.serve()
	return s, nil
}

// Close stops the server listening.
func (s *Server) Close() error {
	if err := s.listener.Close(); err != nil {
		return fmt.Errorf("sftptest: close: %w", err)
	}
	return nil
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
	f := failing{s, h.FilePut, h.FileCmd, h.FileList, h.FileGet}
	h.FilePut, h.FileCmd, h.FileList, h.FileGet = f, f, f, f
	return h
}

type failing struct {
	s    *Server
	put  sftp.FileWriter
	cmd  sftp.FileCmder
	list sftp.FileLister
	get  sftp.FileReader
}

func (f failing) Filelist(r *sftp.Request) (sftp.ListerAt, error) {
	if f.s.FailList && r.Method == "List" {
		return nil, errors.New("permission denied")
	}
	l, err := f.list.Filelist(r)
	if err != nil {
		return nil, fmt.Errorf("sftptest: %s %s: %w", r.Method, r.Filepath, err)
	}
	return l, nil
}

func (f failing) Fileread(r *sftp.Request) (io.ReaderAt, error) {
	ra, err := f.get.Fileread(r)
	if errors.Is(err, os.ErrNotExist) {
		return nil, os.ErrNotExist // as a real server answers: the SFTP layer turns it into SSH_FX_NO_SUCH_FILE, which it does not for a wrapped error
	}
	if err != nil {
		return nil, fmt.Errorf("sftptest: open %s: %w", r.Filepath, err)
	}
	if f.s.FailRead {
		return badReader{}, nil
	}
	return ra, nil
}

type badReader struct{}

func (badReader) ReadAt([]byte, int64) (int, error) { return 0, errors.New("connection reset") }

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
	if f.s.FailRemove && r.Method == "Remove" {
		return errors.New("remove refused")
	}
	if err := f.cmd.Filecmd(r); err != nil {
		return fmt.Errorf("sftptest: %s %s: %w", r.Method, r.Filepath, err)
	}
	if r.Method == "Rename" && f.s.Stored != nil {
		f.s.Stored(r.Target)
	}
	return nil
}

type badWriter struct{}

func (badWriter) WriteAt([]byte, int64) (int, error) { return 0, errors.New("quota exceeded") }

// Read is a file of the server's file system, up to 64 KiB.
func (s *Server) Read(tb testing.TB, path string) (string, bool) {
	tb.Helper()
	b, err := s.ReadFile(path, 64<<10)
	return string(b), err == nil
}

// ReadFile is a file of the server's file system, up to limit bytes.
func (s *Server) ReadFile(path string, limit int) ([]byte, error) {
	r := sftp.NewRequest("Get", path)
	r.Flags = 1 // SSH_FXF_READ
	ra, err := s.files.FileGet.Fileread(r)
	if err != nil {
		return nil, fmt.Errorf("sftptest: read %s: %w", path, err)
	}
	buf := make([]byte, limit)
	n, _ := ra.ReadAt(buf, 0) // io.EOF after a short file
	return buf[:n], nil
}

// Hidden are the names of the hidden files in dir, such as an upload's
// copy left behind.
func (s *Server) Hidden(tb testing.TB, dir string) []string {
	tb.Helper()
	l, err := s.files.FileList.Filelist(sftp.NewRequest("List", dir))
	if err != nil {
		tb.Fatal(err)
	}
	infos := make([]os.FileInfo, 1000)
	n, _ := l.ListAt(infos, 0) // io.EOF once the listing ends
	var out []string
	for _, fi := range infos[:n] {
		if strings.HasPrefix(fi.Name(), ".") {
			out = append(out, fi.Name())
		}
	}
	return out
}

// Put writes a file to the server's file system, as the bank leaves one;
// its directory must exist (Mkdir).
func (s *Server) Put(tb testing.TB, path, content string) {
	tb.Helper()
	r := sftp.NewRequest("Put", path)
	r.Flags = 0x02 | 0x08 | 0x10 // SSH_FXF_WRITE, CREAT and TRUNC
	w, err := s.files.FilePut.Filewrite(r)
	if err != nil {
		tb.Fatal(err)
	}
	if _, err := w.WriteAt([]byte(content), 0); err != nil {
		tb.Fatal(err)
	}
}

// Mkdir makes a directory on the server's file system.
func (s *Server) Mkdir(tb testing.TB, path string) {
	tb.Helper()
	if err := s.MakeDir(path); err != nil {
		tb.Fatal(err)
	}
}

// MakeDir makes a directory on the server's file system.
func (s *Server) MakeDir(path string) error {
	if err := s.files.FileCmd.Filecmd(sftp.NewRequest("Mkdir", path)); err != nil {
		return fmt.Errorf("sftptest: mkdir %s: %w", path, err)
	}
	return nil
}
