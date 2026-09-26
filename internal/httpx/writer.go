// Package httpx holds the http helpers shared by binder, session, server
// and oauth.
package httpx

import (
	"bufio"
	"net"
	"net/http"
	"strings"
)

// TrackingWriter records whether the response has started: binder uses it to
// report handler errors returned after a response went out, session to know
// cookies can no longer be set, server to log the status and size. Unwrap
// keeps http.ResponseController working for deadlines, hijacking and the
// like.
type TrackingWriter struct {
	http.ResponseWriter
	started bool
	status  int
	written int64

	// BeforeStart, when set, runs once just before the response headers go
	// out, while they can still be changed.
	BeforeStart func()
}

// start marks the response started with status, running BeforeStart the
// first time.
func (t *TrackingWriter) start(status int) {
	if t.started {
		return
	}
	t.started, t.status = true, status
	if t.BeforeStart != nil {
		t.BeforeStart()
	}
}

// Started reports whether the response headers have gone out.
func (t *TrackingWriter) Started() bool { return t.started }

// Status is the status the response started with, 101 for a hijacked
// connection, or 0 before it started.
func (t *TrackingWriter) Status() int { return t.status }

// Written is the number of body bytes written.
func (t *TrackingWriter) Written() int64 { return t.written }

func (t *TrackingWriter) WriteHeader(code int) {
	// 1xx informational headers do not start the response, except 101 which
	// hands the connection over.
	if code >= 200 || code == http.StatusSwitchingProtocols {
		t.start(code)
	}
	t.ResponseWriter.WriteHeader(code)
}

func (t *TrackingWriter) Write(p []byte) (int, error) {
	t.start(http.StatusOK)
	n, err := t.ResponseWriter.Write(p)
	t.written += int64(n)
	return n, err
}

// Flush marks the response started even when the wrapped writer can't flush,
// deliberately: net/http sends the headers on the first flush, so the caller
// asked for them to go out and must not rely on changing them afterwards. It
// must stay a method: http.ResponseController would otherwise flush the
// wrapped writer through Unwrap, sending the headers without BeforeStart.
func (t *TrackingWriter) Flush() {
	t.start(http.StatusOK)
	_ = http.NewResponseController(t.ResponseWriter).Flush()
}

// Hijack hands the connection over, e.g. for a WebSocket upgrade. It must
// stay a method: gorilla/websocket type-asserts http.Hijacker rather than
// going through Unwrap. A hijacked response counts as started, without
// BeforeStart, since its headers are written by whoever took the connection.
func (t *TrackingWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, brw, err := http.NewResponseController(t.ResponseWriter).Hijack()
	if err == nil && !t.started {
		t.started, t.status = true, http.StatusSwitchingProtocols
	}
	return conn, brw, err
}

func (t *TrackingWriter) Unwrap() http.ResponseWriter { return t.ResponseWriter }

// IsLocalPath reports whether p is a path on the same host, safe to redirect
// to: it starts with a single "/", and holds no backslash or control
// character, which browsers would turn into "//", another host.
func IsLocalPath(p string) bool {
	if !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") {
		return false
	}
	return !strings.ContainsFunc(p, func(c rune) bool { return c < 0x20 || c == 0x7f || c == '\\' })
}
