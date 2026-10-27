package worker

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/seppaleinen/infermesh/internal/routeraddr"
	"github.com/seppaleinen/infermesh/internal/wsutil"
)

// RouterBaseFromListenAddr derives the dev-mode HTTP registration base URL
// from a router listen address:
//
//	":8080", "0.0.0.0:8080", "localhost:8080" -> "http://127.0.0.1:8080"
//	"127.0.0.1:9000" -> "http://127.0.0.1:9000"
//	"myhost:8080" -> "http://myhost:8080"
//
// The scheme is always http (combined mode is dev-only). URL construction
// delegates to pkg/routeraddr.Normalize (issue #103 R2).
func RouterBaseFromListenAddr(addr string) (string, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("invalid listen address %q: %w", addr, err)
	}
	if p, err := strconv.Atoi(port); err != nil || p < 1 || p > 65535 {
		return "", fmt.Errorf("invalid port in listen address %q", addr)
	}
	if isLoopbackHost(host) {
		host = "127.0.0.1"
	}
	return routeraddr.Normalize(net.JoinHostPort(host, port)), nil
}

// isLoopbackHost reports whether a listen host is empty (any), the wildcard
// "0.0.0.0", or a loopback address.
func isLoopbackHost(host string) bool {
	switch host {
	case "", "0.0.0.0", "localhost", "127.0.0.1", "::1":
		return true
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return true
	}
	return false
}

// relayReachable dials the relay URL with a short timeout to decide whether
// the worker should register via the broker. A failed dial does not abort
// the worker: on a host with both a relay and a direct router URL (the
// Docker Compose relay profile), the worker falls back to HTTP registration;
// on a true outbound-only host only the relay URL is set, so the worker
// stays relay-only and the dial failure is logged as a startup warning.
//
// The dial is fire-and-forget: the returned connection is closed immediately
// after a successful handshake so the real WSRegisterLoop can own the socket.
func relayReachable(ctx context.Context, relayURL string) bool {
	dialCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	conn, err := wsutil.Dial(dialCtx, relayURL, "")
	if err != nil {
		return false
	}
	_ = wsutil.Close(conn)
	return true
}
