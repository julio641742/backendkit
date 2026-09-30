package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// logRecorder replaces the default slog logger for the test, so the tests in
// this package must not run in parallel.
type logRecorder struct {
	mu      sync.Mutex
	records []slog.Record
}

func recordLogs(t *testing.T) *logRecorder {
	t.Helper()
	rec := &logRecorder{}
	old := slog.Default()
	slog.SetDefault(slog.New(rec))
	t.Cleanup(func() { slog.SetDefault(old) })
	return rec
}

func (l *logRecorder) Enabled(context.Context, slog.Level) bool { return true }
func (l *logRecorder) WithAttrs([]slog.Attr) slog.Handler       { return l }
func (l *logRecorder) WithGroup(string) slog.Handler            { return l }

func (l *logRecorder) Handle(_ context.Context, r slog.Record) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.records = append(l.records, r)
	return nil
}

// attrs returns the attributes of every record with message msg.
func (l *logRecorder) attrs(msg string) []map[string]slog.Value {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []map[string]slog.Value
	for _, r := range l.records {
		if r.Message != msg {
			continue
		}
		m := map[string]slog.Value{}
		r.Attrs(func(a slog.Attr) bool {
			m[a.Key] = a.Value
			return true
		})
		out = append(out, m)
	}
	return out
}

const (
	requestMsg = "http request"
	panicMsg   = "server: handler panicked"
)

func TestLog(t *testing.T) {
	logs := recordLogs(t)
	h := Log(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("hello"))
	}))
	req := httptest.NewRequest(http.MethodPost, "/oauth/callback?code=secret", nil)
	h.ServeHTTP(httptest.NewRecorder(), req)

	got := logs.attrs(requestMsg)
	if len(got) != 1 {
		t.Fatalf("logged %d requests, want 1", len(got))
	}
	a := got[0]
	if a["method"].String() != "POST" || a["path"].String() != "/oauth/callback" ||
		a["status"].Int64() != http.StatusCreated || a["bytes"].Int64() != 5 || a["remote"].String() != req.RemoteAddr {
		t.Errorf("attrs = %v", a)
	}
	for _, v := range a {
		if strings.Contains(v.String(), "secret") {
			t.Errorf("the query string was logged: %v", a)
		}
	}
}

func TestRecover(t *testing.T) {
	logs := recordLogs(t)
	h := Log(Recover(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Content-Length", "100")
		w.Header().Add("Set-Cookie", "session=new")
		panic("boom")
	})))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusInternalServerError ||
		rec.Header().Get("Content-Type") != "application/json; charset=utf-8" ||
		!strings.Contains(rec.Body.String(), `"error":"Internal Server Error"`) {
		t.Errorf("response = %d %v %q, want the 500 envelope", rec.Code, rec.Header(), rec.Body.String())
	}
	if rec.Header().Get("Content-Length") != "" {
		t.Error("the handler's Content-Length was kept")
	}
	if rec.Header().Get("Set-Cookie") != "session=new" {
		t.Errorf("Set-Cookie = %q, want the session's cookie kept", rec.Header().Get("Set-Cookie"))
	}

	panics := logs.attrs(panicMsg)
	if len(panics) != 1 || panics[0]["panic"].String() != "boom" || !panickedIn(panics[0], "TestRecover.func1") {
		t.Errorf("panic logs = %v", panics)
	}
	if reqs := logs.attrs(requestMsg); len(reqs) != 1 || reqs[0]["status"].Int64() != http.StatusInternalServerError {
		t.Errorf("request logs = %v, want one 500", reqs)
	}
}

// panickedIn reports whether the logged stack starts at fn, the handler
// that panicked, rather than in Recover, Log or the runtime.
func panickedIn(attrs map[string]slog.Value, fn string) bool {
	frames, ok := attrs["stack"].Any().([]string)
	return ok && len(frames) > 0 && strings.Contains(frames[0], "server."+fn+" (")
}

// mustPanicWith runs fn and checks it panics with want.
func mustPanicWith(t *testing.T, want any, fn func()) {
	t.Helper()
	defer func() {
		if v := recover(); v != want {
			t.Errorf("panic = %v, want %v", v, want)
		}
	}()
	fn()
}

// A panic once the body is under way aborts the connection rather than
// ending a truncated response cleanly; the request is still logged.
func TestRecoverAfterResponseStarted(t *testing.T) {
	logs := recordLogs(t)
	h := Log(Recover(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("partial"))
		panic("boom")
	})))
	mustPanicWith(t, http.ErrAbortHandler, func() {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	})
	if n := len(logs.attrs(panicMsg)); n != 1 {
		t.Errorf("logged %d panics, want 1", n)
	}
	if reqs := logs.attrs(requestMsg); len(reqs) != 1 || reqs[0]["status"].Int64() != http.StatusOK {
		t.Errorf("request logs = %v, want one 200", reqs)
	}
}

// End to end: the client sees the truncation as an error.
func TestRecoverAbortsTruncatedResponse(t *testing.T) {
	recordLogs(t)
	ts := httptest.NewServer(Recover(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("partial"))
		http.NewResponseController(w).Flush()
		panic("boom")
	})))
	defer ts.Close()

	res, err := ts.Client().Get(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	if _, err := io.ReadAll(res.Body); err == nil {
		t.Error("the truncated body read as complete")
	}
}

func TestRecoverPassesAbortOn(t *testing.T) {
	logs := recordLogs(t)
	h := Recover(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic(http.ErrAbortHandler)
	}))
	mustPanicWith(t, http.ErrAbortHandler, func() {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	})
	if n := len(logs.attrs(panicMsg)); n != 0 {
		t.Errorf("logged %d panics, want none", n)
	}
}

// With Recover at the root and Log inside it, the panic is still logged as
// a 500, and Recover still logs the handler's stack.
func TestLogInsideRecover(t *testing.T) {
	logs := recordLogs(t)
	h := Recover(Log(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	})))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code = %d, want 500", rec.Code)
	}
	if reqs := logs.attrs(requestMsg); len(reqs) != 1 || reqs[0]["status"].Int64() != http.StatusInternalServerError {
		t.Errorf("request logs = %v, want one 500", reqs)
	}
	if panics := logs.attrs(panicMsg); len(panics) != 1 || !panickedIn(panics[0], "TestLogInsideRecover.func1") {
		t.Errorf("panic logs = %v, want the handler's stack", panics)
	}
}
