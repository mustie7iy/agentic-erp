package sftpclient_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/pem"
	"errors"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/mustie7iy/agentic-erp/internal/platform/sftpclient"
	"github.com/mustie7iy/agentic-erp/internal/platform/sftpclient/sftptest"
)

// TestUpload: the file arrives whole under its name, whichever of the
// server's host keys of several types is the one pinned.
func TestUpload(t *testing.T) {
	client := sftptest.Signer(t)
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	rsaHost, ecHost, edHost := sftptest.SignerOf(t, rsaKey), sftptest.SignerOf(t, ecKey), sftptest.Signer(t)
	for name, keys := range map[string][]ssh.Signer{"ed25519": {edHost, rsaHost, ecHost}, "rsa": {rsaHost, ecHost, edHost},
		"ecdsa": {ecHost, rsaHost, edHost}} {
		t.Run(name, func(t *testing.T) {
			b := sftptest.NewServer(t, client.PublicKey(), func(b *sftptest.Server) { b.HostSigners = keys })
			if err := b.Endpoint(client).Upload(context.Background(), "pain.xml", []byte("<Document/>")); err != nil {
				t.Fatalf("Upload: %v", err)
			}
			if got, ok := b.Read(t, "/pain.xml"); got != "<Document/>" {
				t.Errorf("uploaded file: got %q (found %v), want <Document/>", got, ok)
			}
			if hidden := b.Hidden(t, "/"); len(hidden) != 0 {
				t.Errorf("partial files: got %v left behind, want none", hidden)
			}
		})
	}
}

func TestUploadFailures(t *testing.T) {
	client := sftptest.Signer(t)
	keep := func(e sftpclient.Endpoint) sftpclient.Endpoint { return e }
	for name, c := range map[string]struct {
		configure func(*sftptest.Server)
		endpoint  func(sftpclient.Endpoint) sftpclient.Endpoint
		file      string
		want      string
	}{
		"another host key":   {func(*sftptest.Server) {}, func(e sftpclient.Endpoint) sftpclient.Endpoint { e.HostKey = sftptest.Signer(t).PublicKey(); return e }, "f.xml", "sign in"},
		"unknown client":     {func(*sftptest.Server) {}, func(e sftpclient.Endpoint) sftpclient.Endpoint { e.Signer = sftptest.Signer(t); return e }, "f.xml", "sign in"},
		"nobody listening":   {func(*sftptest.Server) {}, func(e sftpclient.Endpoint) sftpclient.Endpoint { e.Addr = "127.0.0.1:1"; return e }, "f.xml", "connect"},
		"no sftp subsystem":  {func(b *sftptest.Server) { b.NoSubsystem = true }, keep, "f.xml", "start sftp"},
		"create refused":     {func(b *sftptest.Server) { b.FailOpen = true }, keep, "f.xml", "create /.partial-"},
		"write refused":      {func(b *sftptest.Server) { b.FailWrite = true }, keep, "f.xml", "write /.partial-"},
		"rename refused":     {func(b *sftptest.Server) { b.FailRename = true }, keep, "f.xml", "-f.xml to /f.xml"},
		"a path":             {func(*sftptest.Server) {}, keep, "../f.xml", "not a file name"},
		"no name":            {func(*sftptest.Server) {}, keep, "", "not a file name"},
		"the parent":         {func(*sftptest.Server) {}, keep, "..", "not a file name"},
		"no pinned host key": {func(*sftptest.Server) {}, func(e sftpclient.Endpoint) sftpclient.Endpoint { e.HostKey = nil; return e }, "f.xml", "no pinned host key"},
		"no client key":      {func(*sftptest.Server) {}, func(e sftpclient.Endpoint) sftpclient.Endpoint { e.Signer = nil; return e }, "f.xml", "no client key"},
	} {
		t.Run(name, func(t *testing.T) {
			b := sftptest.NewServer(t, client.PublicKey(), c.configure)
			err := c.endpoint(b.Endpoint(client)).Upload(context.Background(), c.file, []byte("x"))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("got %v, want an error to %s", err, c.want)
			}
			if _, ok := b.Read(t, "/f.xml"); ok {
				t.Error("file: got one, want none")
			}
		})
	}
}

// TestUploadExisting: a file of the name in the directory is left as it
// is, the upload's own copy removed, and the caller told (ErrExists).
func TestUploadExisting(t *testing.T) {
	client := sftptest.Signer(t)
	b := sftptest.NewServer(t, client.PublicKey(), func(*sftptest.Server) {})
	b.Put(t, "/f.xml", "first")
	err := b.Endpoint(client).Upload(context.Background(), "f.xml", []byte("second"))
	if !errors.Is(err, sftpclient.ErrExists) {
		t.Fatalf("got %v, want ErrExists", err)
	}
	if got, _ := b.Read(t, "/f.xml"); got != "first" {
		t.Errorf("file: got %q, want the first left as it was", got)
	}
	if hidden := b.Hidden(t, "/"); len(hidden) != 0 {
		t.Errorf("partial files: got %v left behind, want none", hidden)
	}
}

// TestUploadCancelled: a server that never answers does not hold the caller
// past its context.
func TestUploadCancelled(t *testing.T) {
	client := sftptest.Signer(t)
	b := sftptest.NewServer(t, client.PublicKey(), func(b *sftptest.Server) { b.Silent = true })
	ctx, cancel := context.WithCancel(context.Background())
	go func() { <-b.Accepted; cancel() }()
	if err := b.Endpoint(client).Upload(ctx, "f.xml", []byte("x")); err == nil || !strings.Contains(err.Error(), "sign in") {
		t.Errorf("got %v, want the sign-in cut off", err)
	}
}

func TestParse(t *testing.T) {
	s := sftptest.Signer(t)
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
		if err := cert.SignCert(rand.Reader, sftptest.Signer(t)); err != nil {
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

// TestFetch: the bank's statements are listed in name order without hidden
// files or directories, read whole or cut past the limit, and moved once:
// a move whose acknowledgement was lost, or of the same file delivered
// again, ends with the file at its target once.
func TestFetch(t *testing.T) {
	client := sftptest.Signer(t)
	b := sftptest.NewServer(t, client.PublicKey(), func(*sftptest.Server) {})
	b.Mkdir(t, "/in")
	b.Mkdir(t, "/in/sub")
	b.Mkdir(t, "/archive")
	b.Put(t, "/in/b.sta", "second")
	b.Put(t, "/in/a.sta", "first")
	b.Put(t, "/in/.partial-c.sta", "still coming")
	e, ctx := b.Endpoint(client), context.Background()
	names, err := e.List(ctx, "/in")
	if err != nil || strings.Join(names, ",") != "a.sta,b.sta" {
		t.Fatalf("List: got %v %v, want a.sta,b.sta", names, err)
	}
	t.Run("read", func(t *testing.T) {
		for limit, want := range map[int64]string{5: "first", 3: "firs"} {
			if got, err := e.Read(ctx, "/in", "a.sta", limit); err != nil || string(got) != want {
				t.Errorf("Read with limit %d: got %q %v, want %q", limit, got, err, want)
			}
		}
	})
	t.Run("move", func(t *testing.T) {
		if err := e.Move(ctx, "/in/a.sta", "/archive/a.sta"); err != nil {
			t.Fatalf("Move: %v", err)
		}
		if err := e.Move(ctx, "/in/a.sta", "/archive/a.sta"); err != nil {
			t.Errorf("Move again: got %v, want it moved before", err)
		}
		b.Put(t, "/in/a.sta", "first")
		if err := e.Move(ctx, "/in/a.sta", "/archive/a.sta"); err != nil {
			t.Errorf("Move of the same file again: got %v, want it removed", err)
		}
		if _, ok := b.Read(t, "/in/a.sta"); ok {
			t.Error("the same file again: got it left in place, want it removed")
		}
		if got, _ := b.Read(t, "/archive/a.sta"); got != "first" {
			t.Errorf("archived: got %q, want first", got)
		}
	})
}

func TestFetchFailures(t *testing.T) {
	client := sftptest.Signer(t)
	setup := func(t *testing.T, configure func(*sftptest.Server)) (sftpclient.Endpoint, *sftptest.Server) {
		t.Helper()
		b := sftptest.NewServer(t, client.PublicKey(), configure)
		b.Put(t, "/a.sta", "first")
		b.Put(t, "/done", "first")
		return b.Endpoint(client), b
	}
	ctx := context.Background()
	for name, c := range map[string]struct {
		configure func(*sftptest.Server)
		call      func(e sftpclient.Endpoint) error
		want      string
	}{
		"list refused":  {func(b *sftptest.Server) { b.FailList = true }, func(e sftpclient.Endpoint) error { _, err := e.List(ctx, "/"); return err }, "list /"},
		"no directory":  {func(*sftptest.Server) {}, func(e sftpclient.Endpoint) error { _, err := e.List(ctx, "/none"); return err }, "list /none"},
		"list no key":   {func(*sftptest.Server) {}, func(e sftpclient.Endpoint) error { e.Signer = nil; _, err := e.List(ctx, "/"); return err }, "no client key"},
		"read refused":  {func(b *sftptest.Server) { b.FailRead = true }, func(e sftpclient.Endpoint) error { _, err := e.Read(ctx, "/", "a.sta", 9); return err }, "read /a.sta"},
		"no such file":  {func(*sftptest.Server) {}, func(e sftpclient.Endpoint) error { _, err := e.Read(ctx, "/", "b.sta", 9); return err }, "open /b.sta"},
		"read a path":   {func(*sftptest.Server) {}, func(e sftpclient.Endpoint) error { _, err := e.Read(ctx, "/", "../a.sta", 9); return err }, "not a file name"},
		"read a folder": {func(b *sftptest.Server) { b.Mkdir(t, "/d") }, func(e sftpclient.Endpoint) error { _, err := e.Read(ctx, "/", "d", 9); return err }, "/d"},
		"rename refused, nothing at the target": {func(b *sftptest.Server) { b.FailRename = true },
			func(e sftpclient.Endpoint) error { return e.Move(ctx, "/a.sta", "/b.sta") }, "rename /a.sta to /b.sta"},
		"remove refused": {func(b *sftptest.Server) { b.FailRemove = true },
			func(e sftpclient.Endpoint) error { return e.Move(ctx, "/a.sta", "/done") }, "remove /a.sta, already at /done"},
	} {
		t.Run(name, func(t *testing.T) {
			e, _ := setup(t, c.configure)
			if err := c.call(e); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("got %v, want an error naming %s", err, c.want)
			}
		})
	}
}
