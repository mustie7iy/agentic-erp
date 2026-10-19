package sftpclient_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/pem"
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
			if _, ok := b.Read(t, "/.partial-pain.xml"); ok {
				t.Error("partial file: got one left behind, want none")
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
		"create refused":     {func(b *sftptest.Server) { b.FailOpen = true }, keep, "f.xml", "create /.partial-f.xml"},
		"write refused":      {func(b *sftptest.Server) { b.FailWrite = true }, keep, "f.xml", "write /.partial-f.xml"},
		"rename refused":     {func(b *sftptest.Server) { b.FailRename = true }, keep, "f.xml", "rename /.partial-f.xml to /f.xml"},
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
