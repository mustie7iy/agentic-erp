// Package sftpclient delivers files to a bank's SFTP server (ADR-0038).
// The server is known by its pinned host key, never trusted on first use,
// and the client signs in with a key: there is no password authentication.
// A file appears under its name complete or not at all: it is written under
// a hidden name and renamed, as payments.DirConnector does on a directory.
package sftpclient

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path"
	"strings"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// Endpoint is a directory on an SFTP server and how to sign in to it.
type Endpoint struct {
	Addr    string        // host:port
	User    string        // the account the bank set up
	HostKey ssh.PublicKey // the server's key, pinned
	Signer  ssh.Signer    // the client's private key, from a secret
	Dir     string        // the directory the bank collects from
}

// ParseHostKey reads a server's public key in authorized_keys form, for
// example "ssh-ed25519 AAAA…". A certificate is refused: the key is pinned,
// not vouched for by an authority.
func ParseHostKey(line string) (ssh.PublicKey, error) {
	key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(line))
	if err != nil {
		return nil, fmt.Errorf("sftpclient: parse host key: %w", err)
	}
	if _, isCert := key.(*ssh.Certificate); isCert {
		return nil, fmt.Errorf("sftpclient: host key %s is a certificate, not a key to pin", key.Type())
	}
	return key, nil
}

// ParseSigner reads the client's private key (PEM or OpenSSH form,
// unencrypted: the secret store protects it).
func ParseSigner(pem []byte) (ssh.Signer, error) {
	s, err := ssh.ParsePrivateKey(pem)
	if err != nil {
		return nil, fmt.Errorf("sftpclient: parse private key: %w", err)
	}
	return s, nil
}

// Upload writes content to name, a plain file name, in the endpoint's
// directory. Cancelling ctx closes the connection, so a server that stops
// answering does not hold the caller. SFTP's rename refuses an existing
// target, so a file uploaded once is not overwritten by a retry under its
// name: telling a retry after a lost acknowledgement from a new file is the
// caller's, by content hash (ADR-0038, T44c).
func (e Endpoint) Upload(ctx context.Context, name string, content []byte) error {
	if name == "" || name == "." || name == ".." || strings.Contains(name, "/") {
		return fmt.Errorf("sftpclient: %q is not a file name", name)
	}
	if e.HostKey == nil || e.Signer == nil {
		return fmt.Errorf("sftpclient: %s has no pinned host key or no client key", e.Addr) // fail closed, never trust on first use
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", e.Addr)
	if err != nil {
		return fmt.Errorf("sftpclient: connect to %s: %w", e.Addr, err)
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() }) // unblocks a hung exchange; the exchange then reports the error
	defer stop()
	c, chans, reqs, err := ssh.NewClientConn(conn, e.Addr, &ssh.ClientConfig{User: e.User, Auth: []ssh.AuthMethod{ssh.PublicKeys(e.Signer)},
		HostKeyCallback: ssh.FixedHostKey(e.HostKey), HostKeyAlgorithms: hostKeyAlgorithms(e.HostKey)})
	if err != nil {
		_ = conn.Close() // the handshake failed; its error is the one reported
		return fmt.Errorf("sftpclient: sign in to %s as %s: %w", e.Addr, e.User, err)
	}
	client := ssh.NewClient(c, chans, reqs)
	defer func() { _ = client.Close() }() // the upload's outcome is decided by the rename below
	s, err := sftp.NewClient(client)
	if err != nil {
		return fmt.Errorf("sftpclient: start sftp on %s: %w", e.Addr, err)
	}
	defer func() { _ = s.Close() }() // as for client
	return e.put(s, name, content)
}

// hostKeyAlgorithms asks the server for the pinned key: a server with keys
// of several types would otherwise present the one the client prefers, and
// the pin would refuse it. An RSA key signs with SHA-2, never SHA-1.
func hostKeyAlgorithms(k ssh.PublicKey) []string {
	if k.Type() == ssh.KeyAlgoRSA {
		return []string{ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256}
	}
	return []string{k.Type()}
}

// put writes the file under a hidden name, then renames it into place.
func (e Endpoint) put(s *sftp.Client, name string, content []byte) error {
	tmp, final := path.Join(e.Dir, ".partial-"+name), path.Join(e.Dir, name)
	f, err := s.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC)
	if err != nil {
		return fmt.Errorf("sftpclient: create %s: %w", tmp, err)
	}
	_, err = f.Write(content)
	if err = errors.Join(err, f.Close()); err != nil {
		return fmt.Errorf("sftpclient: write %s: %w", tmp, err)
	}
	if err := s.Rename(tmp, final); err != nil {
		return fmt.Errorf("sftpclient: rename %s to %s: %w", tmp, final, err)
	}
	return nil
}
