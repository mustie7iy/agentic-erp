// Command natsstub is the pilot walkthrough's event bus
// (scripts/walkthrough.sh): the embedded NATS JetStream server the tests
// of the outbox relay and the webhook fan-out run (natstest, ADR-0007), on
// a loopback port of its own, its store in a directory the walkthrough
// removes. erp-server connects to it with ERP_NATS_URL, as to any bus in
// development, so the walkthrough's events are relayed, fanned out to
// webhook subscriptions and delivered (ADR-0041 As built, product owner
// 2026-11-04). It writes its URL to the ready file once it accepts
// connections (the walkthrough waits for the file) and runs until it is
// stopped.
//
//	natsstub -addr 127.0.0.1:18222 -store nats -ready nats.url
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/nats-io/nats-server/v2/server"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:18222", "where to listen: a loopback address and port")
	store := flag.String("store", "", "the JetStream store directory")
	ready := flag.String("ready", "", "where to write the server's URL once it accepts connections")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, *addr, *store, *ready)
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "natsstub:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, addr, store, ready string) error {
	host, p, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("addr: %w", err)
	}
	port, err := strconv.Atoi(p)
	if err != nil {
		return fmt.Errorf("addr: %w", err)
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return errors.New("addr: the walkthrough's bus listens on a loopback address only")
	}
	s, err := server.NewServer(&server.Options{Host: host, Port: port, JetStream: true, StoreDir: store, NoLog: true, NoSigs: true})
	if err != nil {
		return fmt.Errorf("server: %w", err)
	}
	go s.Start()
	defer s.Shutdown()
	if !s.ReadyForConnections(10 * time.Second) {
		return errors.New("not ready after ten seconds")
	}
	if err := os.WriteFile(ready, []byte(s.ClientURL()+"\n"), 0o600); err != nil { // #nosec G703 -- the walkthrough names its own file
		return fmt.Errorf("ready file: %w", err)
	}
	fmt.Println("natsstub: listening on", s.ClientURL())
	<-ctx.Done()
	return nil
}
