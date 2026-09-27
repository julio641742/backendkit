// Package server runs the backend's HTTP servers. Recover wraps the whole
// router, Log the routes worth logging, and Run serves until shutdown:
//
//	handler := server.Recover(server.Log(sessions(mux)))
//	srv := &http.Server{Addr: ":3000", Handler: handler, TLSConfig: tlsConfig}
//
//	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
//	defer stop()
//	err := server.Run(ctx, srv)
package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"
)

const (
	readHeaderTimeout = 10 * time.Second
	idleTimeout       = 2 * time.Minute
	shutdownTimeout   = 10 * time.Second
)

// Run serves each server until ctx is cancelled or one of them fails, then
// shuts them all down within 10s. A server with a TLSConfig, which must hold
// the certificate, is served over TLS (development, with localtls); one
// without serves plain HTTP behind a TLS-terminating proxy such as Caddy.
//
// Shutdown has two steps. Requests in flight finish first, with their
// contexts intact. Then the request contexts are cancelled and Run waits for
// the handlers still running: those of hijacked connections such as
// WebSockets, which http.Server.Shutdown doesn't track. Such a handler must
// return once r.Context() is done, closing its connection cleanly first.
//
// Run sets each server's BaseContext and wraps its Handler, and sets
// ReadHeaderTimeout (10s) and IdleTimeout (2m) when they are zero. Run
// returns nil after ctx is cancelled, or the error of the server that failed,
// joined with any shutdown errors.
func Run(ctx context.Context, servers ...*http.Server) error {
	// Request contexts outlive ctx, so a signal doesn't cancel the requests
	// in flight; they are cancelled once those have finished.
	baseCtx, cancelRequests := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelRequests()
	var handlers sync.WaitGroup

	errc := make(chan error, len(servers))
	for _, srv := range servers {
		if srv.ReadHeaderTimeout == 0 {
			srv.ReadHeaderTimeout = readHeaderTimeout
		}
		if srv.IdleTimeout == 0 {
			srv.IdleTimeout = idleTimeout
		}
		srv.BaseContext = func(net.Listener) context.Context { return baseCtx }
		next := srv.Handler
		if next == nil {
			next = http.DefaultServeMux
		}
		srv.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			handlers.Add(1)
			defer handlers.Done()
			next.ServeHTTP(w, r)
		})
		go func() {
			if srv.TLSConfig != nil {
				errc <- srv.ListenAndServeTLS("", "")
			} else {
				errc <- srv.ListenAndServe()
			}
		}()
	}

	var errs []error
	select {
	case <-ctx.Done():
	case err := <-errc:
		errs = append(errs, fmt.Errorf("server: %w", err))
	}

	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()

	// Concurrently, so no server keeps accepting while another drains.
	shutdownErrs := make([]error, len(servers))
	var wg sync.WaitGroup
	for i, srv := range servers {
		wg.Go(func() {
			if err := srv.Shutdown(shutdownCtx); err != nil {
				shutdownErrs[i] = fmt.Errorf("server: shut down %s: %w", srv.Addr, err)
			}
		})
	}
	wg.Wait()
	errs = append(errs, shutdownErrs...)

	// Only hijacked connections are left, unless Shutdown timed out, in which
	// case requests may still be starting and the deadline is gone anyway.
	cancelRequests()
	if shutdownCtx.Err() == nil {
		done := make(chan struct{})
		go func() { handlers.Wait(); close(done) }()
		select {
		case <-done:
		case <-shutdownCtx.Done():
			errs = append(errs, errors.New("server: hijacked connections still open after the shutdown timeout"))
		}
	}

	return errors.Join(errs...)
}
