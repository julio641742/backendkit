package server

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// testTLS returns a config holding httptest's self-signed certificate.
func testTLS(t *testing.T) *tls.Config {
	t.Helper()
	ts := httptest.NewTLSServer(http.NotFoundHandler())
	defer ts.Close()
	return &tls.Config{Certificates: ts.TLS.Certificates}
}

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().String()
}

var client = &http.Client{Transport: &http.Transport{
	TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
}}

// start runs Run in the background and waits until addr accepts connections.
func start(t *testing.T, ctx context.Context, addr string, servers ...*http.Server) <-chan error {
	t.Helper()
	runErr := make(chan error, 1)
	go func() { runErr <- Run(ctx, servers...) }()

	deadline := time.Now().Add(5 * time.Second)
	for {
		conn, err := net.Dial("tcp", addr)
		if err == nil {
			_ = conn.Close()
			return runErr
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never came up: %v", addr, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRunGracefulShutdown(t *testing.T) {
	addr := freeAddr(t)
	started, release := make(chan struct{}), make(chan struct{})
	srv := &http.Server{Addr: addr, TLSConfig: testTLS(t), Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		if err := r.Context().Err(); err != nil {
			t.Errorf("request context cancelled while in flight: %v", err)
		}
		_, _ = w.Write([]byte("done"))
	})}
	ctx, cancel := context.WithCancel(context.Background())
	runErr := start(t, ctx, addr, srv)

	type result struct {
		body string
		err  error
	}
	res := make(chan result, 1)
	go func() {
		r, err := client.Get("https://" + addr)
		if err != nil {
			res <- result{err: err}
			return
		}
		defer func() { _ = r.Body.Close() }()
		b, err := io.ReadAll(r.Body)
		res <- result{string(b), err}
	}()

	<-started
	cancel()
	select {
	case err := <-runErr:
		t.Fatalf("Run returned with a request in flight: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)

	if r := <-res; r.err != nil || r.body != "done" {
		t.Errorf("in-flight request = %q, %v; want done", r.body, r.err)
	}
	if err := <-runErr; err != nil {
		t.Errorf("Run = %v, want nil", err)
	}
	if srv.ReadHeaderTimeout != readHeaderTimeout || srv.IdleTimeout != idleTimeout {
		t.Errorf("timeouts = %v, %v; want the defaults", srv.ReadHeaderTimeout, srv.IdleTimeout)
	}
}

// Shutdown doesn't track hijacked connections: Run cancels their request
// context and waits for their handlers to close them.
func TestRunWaitsForHijackedConnections(t *testing.T) {
	addr := freeAddr(t)
	hijacked := make(chan struct{})
	var handlerDone atomic.Bool
	srv := &http.Server{Addr: addr, TLSConfig: testTLS(t), Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Errorf("Hijack: %v", err)
			return
		}
		close(hijacked)
		<-r.Context().Done()
		_, _ = conn.Write([]byte("bye"))
		_ = conn.Close()
		handlerDone.Store(true)
	})}
	ctx, cancel := context.WithCancel(context.Background())
	runErr := start(t, ctx, addr, srv)

	conn, err := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	<-hijacked
	cancel()

	if b, _ := io.ReadAll(conn); string(b) != "bye" {
		t.Errorf("client read %q, want bye", b)
	}
	if err := <-runErr; err != nil {
		t.Errorf("Run = %v, want nil", err)
	}
	if !handlerDone.Load() {
		t.Error("Run returned before the hijacked connection's handler")
	}
}

func TestRunReturnsServerError(t *testing.T) {
	addr := freeAddr(t)
	cfg := testTLS(t)
	keep := &http.Server{Addr: addr, TLSConfig: cfg, ReadHeaderTimeout: 3 * time.Second}
	clash := &http.Server{Addr: addr, TLSConfig: cfg}

	err := Run(context.Background(), keep, clash)
	if err == nil || !strings.Contains(err.Error(), "address already in use") {
		t.Errorf("Run = %v, want address already in use", err)
	}
	if keep.ReadHeaderTimeout != 3*time.Second {
		t.Errorf("ReadHeaderTimeout = %v, want the one set kept", keep.ReadHeaderTimeout)
	}
}

// Behind Caddy, which terminates TLS, servers have no TLSConfig.
func TestRunPlainHTTP(t *testing.T) {
	addr := freeAddr(t)
	srv := &http.Server{Addr: addr, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("plain"))
	})}
	ctx, cancel := context.WithCancel(context.Background())
	runErr := start(t, ctx, addr, srv)

	res, err := http.Get("http://" + addr)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if err != nil || string(b) != "plain" {
		t.Errorf("body = %q, %v; want plain", b, err)
	}

	cancel()
	if err := <-runErr; err != nil {
		t.Errorf("Run = %v, want nil", err)
	}
}
