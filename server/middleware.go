package server

import (
	"fmt"
	"log/slog"
	"net/http"
	"runtime"
	"strings"
	"time"

	"github.com/julio641742/backendkit/httperr"
	"github.com/julio641742/backendkit/internal/httpx"
)

// Log logs each request with slog once its handler returns: the method,
// path, status, body bytes, duration and remote address. The query string is
// left out, since it can carry secrets such as an OAuth code. A hijacked
// connection logs status 101, and a request that was never answered status
// 0. A panic passing through is logged as the 500 Recover turns it into,
// unless the response had started, and then passed on. Leave out the routes
// not worth a line, such as the frontend's files.
func Log(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		tw := &httpx.TrackingWriter{ResponseWriter: w}
		defer func() {
			status := tw.Status()
			// Re-panicking from here keeps the handler's frames on the stack
			// that Recover logs.
			v := recover()
			if v != nil && !tw.Started() {
				status = http.StatusInternalServerError
			}
			slog.LogAttrs(r.Context(), slog.LevelInfo, "http request",
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", status),
				slog.Int64("bytes", tw.Written()),
				slog.Duration("duration", time.Since(start)),
				slog.String("remote", r.RemoteAddr),
			)
			if v != nil {
				panic(v)
			}
		}()
		next.ServeHTTP(tw, r)
	})
}

// Recover answers a panicking handler with the httperr 500 envelope, and
// logs the panic and its stack with slog, as a list of frames that keeps
// the record on one line. Headers the handler set are
// dropped, except Set-Cookie: a session saved before the panic must keep its
// new cookie. When the response has already started, the connection is
// aborted instead (by panicking with http.ErrAbortHandler), so the client
// can't take a truncated body for a whole one. A panic with
// http.ErrAbortHandler itself is passed on without logging.
func Recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tw := &httpx.TrackingWriter{ResponseWriter: w}
		defer func() {
			v := recover()
			if v == nil {
				return
			}
			if v == http.ErrAbortHandler {
				panic(v)
			}
			slog.ErrorContext(r.Context(), "server: handler panicked",
				"method", r.Method,
				"path", r.URL.Path,
				"panic", v,
				"stack", stack(),
			)
			if tw.Started() {
				panic(http.ErrAbortHandler)
			}
			h := tw.Header()
			for k := range h {
				if k != "Set-Cookie" {
					delete(h, k)
				}
			}
			httperr.Write(tw, http.StatusInternalServerError, "")
		}()
		next.ServeHTTP(tw, r)
	})
}

// stack lists the panicking goroutine's frames as "function (file:line)",
// innermost first. It starts at the frame that panicked: everything up to
// the outermost runtime.gopanic is Recover's, or Log passing the panic on,
// and the runtime's own frames are left out.
func stack() []string {
	pcs := make([]uintptr, 64)
	frames := runtime.CallersFrames(pcs[:runtime.Callers(1, pcs)])
	var out []string
	for {
		f, more := frames.Next()
		switch {
		case f.Function == "runtime.gopanic":
			out = out[:0]
		case !strings.HasPrefix(f.Function, "runtime."):
			out = append(out, fmt.Sprintf("%s (%s:%d)", f.Function, f.File, f.Line))
		}
		if !more {
			return out
		}
	}
}
