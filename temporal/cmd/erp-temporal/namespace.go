package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"time"

	"go.temporal.io/api/serviceerror"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/server/common/config"
	"google.golang.org/protobuf/types/known/durationpb"
)

// registerWait bounds how long the namespace's registration waits for the
// frontend to answer after the start; retryEvery is the pause between
// tries. Tests shorten both.
var registerWait, retryEvery = 2 * time.Minute, time.Second

// newNamespaceClient is the SDK's, replaced by tests.
var newNamespaceClient = client.NewNamespaceClient

// ensureNamespace registers the cell's namespace, as the engine's own
// identity (the system worker's certificate), unless it exists. Every node
// tries; the first registers it and the others find it.
func ensureNamespace(ctx context.Context, cfg *config.Config, s settings) error {
	tlsCfg, err := systemTLS(cfg.Global.TLS.SystemWorker)
	if err != nil {
		return fmt.Errorf("namespace %s: %w", s.namespace, err)
	}
	c, err := newNamespaceClient(client.Options{
		HostPort:          cfg.PublicClient.HostPort,
		ConnectionOptions: client.ConnectionOptions{TLS: tlsCfg},
		Logger:            quiet{},
	})
	if err != nil {
		return fmt.Errorf("namespace %s: client: %w", s.namespace, err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(ctx, registerWait)
	defer cancel()
	req := &workflowservice.RegisterNamespaceRequest{
		Namespace:                        s.namespace,
		Description:                      "the cell's workflows (ADR-0012)",
		WorkflowExecutionRetentionPeriod: durationpb.New(s.retention),
	}
	for {
		err = c.Register(ctx, req)
		var exists *serviceerror.NamespaceAlreadyExists
		var unavailable *serviceerror.Unavailable
		switch {
		case err == nil || errors.As(err, &exists):
			return nil
		case !errors.As(err, &unavailable):
			return fmt.Errorf("namespace %s: register: %w", s.namespace, err)
		}
		// The frontend is still starting: try again until the wait ends.
		select {
		case <-ctx.Done():
			return fmt.Errorf("namespace %s: the frontend did not answer: %w", s.namespace, err)
		case <-time.After(retryEvery):
		}
	}
}

// systemTLS is the client configuration of the system worker's section:
// the engine's certificate, the environment's CA, the frontend's name,
// TLS 1.3.
func systemTLS(w config.WorkerTLS) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(w.CertFile, w.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("system worker certificate: %w", err)
	}
	roots := x509.NewCertPool()
	for _, f := range w.Client.RootCAFiles {
		pem, err := os.ReadFile(f)
		if err != nil {
			return nil, fmt.Errorf("system worker CA: %w", err)
		}
		if !roots.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("system worker CA %s: no certificate", f)
		}
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      roots,
		ServerName:   w.Client.ServerName,
		MinVersion:   tls.VersionTLS13,
	}, nil
}

// quiet is the SDK's logger, silenced: the engine logs what matters.
type quiet struct{}

func (quiet) Debug(string, ...any) {}
func (quiet) Info(string, ...any)  {}
func (quiet) Warn(string, ...any)  {}
func (quiet) Error(string, ...any) {}
