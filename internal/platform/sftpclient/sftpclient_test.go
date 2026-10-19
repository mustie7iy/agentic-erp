package sftpclient_test

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"strings"
	"testing"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/mustie7iy/agentic-erp/internal/platform/sftpclient"
)

// bank is an in-process SFTP server on an in-memory file system, accepting
// one client key, that can be told to fail a step.
type bank struct {
	addr        string
	listener    net.Listener
	hostSigners []ssh.Signer // the first is the key the client pins
	clientKey   ssh.PublicKey
	files       sftp.Handlers
	accepted    chan struct{}
	silent      bool // reads the client's version line and never answers
	noSubsystem bool
	failOpen    bool
	failWrite   bool
	failRename  bool
}

func signer(t *testing.T) ssh.Signer {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return signerOf(t, key)
}

func signerOf(t *testing.T, key any) ssh.Signer {
	t.Helper()
	s, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// newBank starts the server for a client key; it stops with the test.
func newBank(t *testing.T, client ssh.PublicKey, configure func(*bank)) *bank {
	t.Helper()
	l, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	b := &bank{addr: l.Addr().String(), listener: l, hostSigners: []ssh.Signer{signer(t)}, clientKey: client, files: sftp.InMemHandler(),
		accepted: make(chan struct{}, 1)}
	configure(b)
	go b.serve()
	return b
}

func (b *bank) serve() {
	cfg := &ssh.ServerConfig{PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		if string(key.Marshal()) != string(b.clientKey.Marshal()) {
			return nil, errors.New("unknown key")
		}
		return &ssh.Permissions{}, nil
	}}
	for _, h := range b.hostSigners {
		cfg.AddHostKey(h)
	}
	for {
		conn, err := b.listener.Accept()
		if err != nil {
			return
		}
		if b.silent {
			go func() {
				_, _ = bufio.NewReader(conn).ReadString('\n') // the client's version line: it now waits for the server's
				b.accepted <- struct{}{}
			}()
			continue
		}
		go b.session(conn, cfg)
	}
}

func (b *bank) session(conn net.Conn, cfg *ssh.ServerConfig) {
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
				ok := r.Type == "subsystem" && !b.noSubsystem
				_ = r.Reply(ok, nil)
				if ok {
					go func() { _ = sftp.NewRequestServer(ch, b.handlers()).Serve(); _ = ch.Close() }() // as a real server ends the session
				}
			}
		}()
	}
}

// handlers are the in-memory file system with the failures asked for.
func (b *bank) handlers() sftp.Handlers {
	h := b.files
	h.FilePut, h.FileCmd = failing{b, h.FilePut, h.FileCmd}, failing{b, h.FilePut, h.FileCmd}
	return h
}

type failing struct {
	b   *bank
	put sftp.FileWriter
	cmd sftp.FileCmder
}

func (f failing) Filewrite(r *sftp.Request) (io.WriterAt, error) {
	if f.b.failOpen {
		return nil, errors.New("disk full")
	}
	w, err := f.put.Filewrite(r)
	if f.b.failWrite {
		return badWriter{}, err
	}
	return w, err
}

func (f failing) Filecmd(r *sftp.Request) error {
	if f.b.failRename && r.Method == "Rename" {
		return errors.New("rename refused")
	}
	return f.cmd.Filecmd(r)
}

type badWriter struct{}

func (badWriter) WriteAt([]byte, int64) (int, error) { return 0, errors.New("quota exceeded") }

// read is a file of the bank's file system.
func (b *bank) read(t *testing.T, path string) (string, bool) {
	t.Helper()
	r := sftp.NewRequest("Get", path)
	r.Flags = 1 // SSH_FXF_READ
	ra, err := b.files.FileGet.Fileread(r)
	if err != nil {
		return "", false
	}
	buf := make([]byte, 64)
	n, _ := ra.ReadAt(buf, 0) // io.EOF after a short file
	return string(buf[:n]), true
}

func (b *bank) endpoint(client ssh.Signer) sftpclient.Endpoint {
	return sftpclient.Endpoint{Addr: b.addr, User: "erp", HostKey: b.hostSigners[0].PublicKey(), Signer: client, Dir: "/"}
}

// TestUpload: the file arrives whole under its name, whichever of the
// server's host keys of several types is the one pinned.
func TestUpload(t *testing.T) {
	client := signer(t)
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	rsaHost, ecHost, edHost := signerOf(t, rsaKey), signerOf(t, ecKey), signer(t)
	for name, keys := range map[string][]ssh.Signer{"ed25519": {edHost, rsaHost, ecHost}, "rsa": {rsaHost, ecHost, edHost},
		"ecdsa": {ecHost, rsaHost, edHost}} {
		t.Run(name, func(t *testing.T) {
			b := newBank(t, client.PublicKey(), func(b *bank) { b.hostSigners = keys })
			if err := b.endpoint(client).Upload(context.Background(), "pain.xml", []byte("<Document/>")); err != nil {
				t.Fatalf("Upload: %v", err)
			}
			if got, ok := b.read(t, "/pain.xml"); got != "<Document/>" {
				t.Errorf("uploaded file: got %q (found %v), want <Document/>", got, ok)
			}
			if _, ok := b.read(t, "/.partial-pain.xml"); ok {
				t.Error("partial file: got one left behind, want none")
			}
		})
	}
}

func TestUploadFailures(t *testing.T) {
	client := signer(t)
	keep := func(e sftpclient.Endpoint) sftpclient.Endpoint { return e }
	for name, c := range map[string]struct {
		configure func(*bank)
		endpoint  func(sftpclient.Endpoint) sftpclient.Endpoint
		file      string
		want      string
	}{
		"another host key":   {func(*bank) {}, func(e sftpclient.Endpoint) sftpclient.Endpoint { e.HostKey = signer(t).PublicKey(); return e }, "f.xml", "sign in"},
		"unknown client":     {func(*bank) {}, func(e sftpclient.Endpoint) sftpclient.Endpoint { e.Signer = signer(t); return e }, "f.xml", "sign in"},
		"nobody listening":   {func(*bank) {}, func(e sftpclient.Endpoint) sftpclient.Endpoint { e.Addr = "127.0.0.1:1"; return e }, "f.xml", "connect"},
		"no sftp subsystem":  {func(b *bank) { b.noSubsystem = true }, keep, "f.xml", "start sftp"},
		"create refused":     {func(b *bank) { b.failOpen = true }, keep, "f.xml", "create /.partial-f.xml"},
		"write refused":      {func(b *bank) { b.failWrite = true }, keep, "f.xml", "write /.partial-f.xml"},
		"rename refused":     {func(b *bank) { b.failRename = true }, keep, "f.xml", "rename /.partial-f.xml to /f.xml"},
		"a path":             {func(*bank) {}, keep, "../f.xml", "not a file name"},
		"no name":            {func(*bank) {}, keep, "", "not a file name"},
		"the parent":         {func(*bank) {}, keep, "..", "not a file name"},
		"no pinned host key": {func(*bank) {}, func(e sftpclient.Endpoint) sftpclient.Endpoint { e.HostKey = nil; return e }, "f.xml", "no pinned host key"},
		"no client key":      {func(*bank) {}, func(e sftpclient.Endpoint) sftpclient.Endpoint { e.Signer = nil; return e }, "f.xml", "no client key"},
	} {
		t.Run(name, func(t *testing.T) {
			b := newBank(t, client.PublicKey(), c.configure)
			err := c.endpoint(b.endpoint(client)).Upload(context.Background(), c.file, []byte("x"))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("got %v, want an error to %s", err, c.want)
			}
			if _, ok := b.read(t, "/f.xml"); ok {
				t.Error("file: got one, want none")
			}
		})
	}
}

// TestUploadCancelled: a server that never answers does not hold the caller
// past its context.
func TestUploadCancelled(t *testing.T) {
	client := signer(t)
	b := newBank(t, client.PublicKey(), func(b *bank) { b.silent = true })
	ctx, cancel := context.WithCancel(context.Background())
	go func() { <-b.accepted; cancel() }()
	if err := b.endpoint(client).Upload(ctx, "f.xml", []byte("x")); err == nil || !strings.Contains(err.Error(), "sign in") {
		t.Errorf("got %v, want the sign-in cut off", err)
	}
}

func TestParse(t *testing.T) {
	s := signer(t)
	t.Run("host key", func(t *testing.T) {
		key, err := sftpclient.ParseHostKey(string(ssh.MarshalAuthorizedKey(s.PublicKey())))
		if err != nil || string(key.Marshal()) != string(s.PublicKey().Marshal()) {
			t.Errorf("got %v, %v, want the key back", key, err)
		}
	})
	t.Run("not a host key", func(t *testing.T) {
		if _, err := sftpclient.ParseHostKey("ssh-ed25519 not-base64"); err == nil {
			t.Error("got none refused, want an error")
		}
	})
	t.Run("a certificate", func(t *testing.T) {
		cert := &ssh.Certificate{Key: s.PublicKey(), CertType: ssh.HostCert, ValidBefore: ssh.CertTimeInfinity}
		if err := cert.SignCert(rand.Reader, signer(t)); err != nil {
			t.Fatal(err)
		}
		if _, err := sftpclient.ParseHostKey(string(ssh.MarshalAuthorizedKey(cert))); err == nil || !strings.Contains(err.Error(), "certificate") {
			t.Errorf("got %v, want the certificate refused", err)
		}
	})
	t.Run("private key", func(t *testing.T) {
		_, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		block, err := ssh.MarshalPrivateKey(priv, "")
		if err != nil {
			t.Fatal(err)
		}
		if got, err := sftpclient.ParseSigner(pem.EncodeToMemory(block)); err != nil || got == nil {
			t.Errorf("got %v, want a signer", err)
		}
	})
	t.Run("a password", func(t *testing.T) {
		if _, err := sftpclient.ParseSigner([]byte("password")); err == nil {
			t.Error("got a password accepted, want an error")
		}
	})
}
