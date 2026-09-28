package httpx

import (
	"bufio"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWriteHeader(t *testing.T) {
	tests := []struct {
		code    int
		started bool
	}{
		{http.StatusContinue, false},
		{http.StatusEarlyHints, false},
		{http.StatusSwitchingProtocols, true},
		{http.StatusOK, true},
		{http.StatusNotFound, true},
	}
	for _, tt := range tests {
		t.Run(http.StatusText(tt.code), func(t *testing.T) {
			rec := httptest.NewRecorder()
			w := &TrackingWriter{ResponseWriter: rec}
			w.WriteHeader(tt.code)
			if w.Started() != tt.started {
				t.Errorf("Started() = %v, want %v", w.Started(), tt.started)
			}
			if want := map[bool]int{true: tt.code}[tt.started]; w.Status() != want {
				t.Errorf("Status() = %d, want %d", w.Status(), want)
			}
		})
	}
}

func TestInformationalThenFinal(t *testing.T) {
	w := &TrackingWriter{ResponseWriter: httptest.NewRecorder()}
	w.WriteHeader(http.StatusEarlyHints)
	w.WriteHeader(http.StatusNotFound)
	if !w.Started() || w.Status() != http.StatusNotFound {
		t.Errorf("Started() = %v, Status() = %d after the final status", w.Started(), w.Status())
	}
}

func TestWriteStarts(t *testing.T) {
	rec := httptest.NewRecorder()
	w := &TrackingWriter{ResponseWriter: rec}
	if w.Started() {
		t.Fatal("started before anything was written")
	}
	if _, err := w.Write([]byte("hi")); err != nil {
		t.Fatal(err)
	}
	if !w.Started() || rec.Body.String() != "hi" {
		t.Errorf("Started() = %v, body = %q", w.Started(), rec.Body.String())
	}
	if _, err := w.Write([]byte(" there")); err != nil {
		t.Fatal(err)
	}
	if w.Status() != http.StatusOK || w.Written() != 8 {
		t.Errorf("Status() = %d, Written() = %d; want 200, 8", w.Status(), w.Written())
	}
}

func TestCopy(t *testing.T) {
	rec := httptest.NewRecorder()
	w := &TrackingWriter{ResponseWriter: rec}
	n, err := io.Copy(w, strings.NewReader("hello"))
	if err != nil || n != 5 {
		t.Fatalf("Copy = %d, %v", n, err)
	}
	if !w.Started() || rec.Body.String() != "hello" {
		t.Errorf("Started() = %v, body = %q", w.Started(), rec.Body.String())
	}
}

// A flush through http.ResponseController must reach Flush, not the wrapped
// writer, or the headers go out unnoticed.
func TestFlush(t *testing.T) {
	rec := httptest.NewRecorder()
	w := &TrackingWriter{ResponseWriter: rec}
	if err := http.NewResponseController(w).Flush(); err != nil {
		t.Fatal(err)
	}
	if !w.Started() || !rec.Flushed {
		t.Errorf("Started() = %v, Flushed = %v", w.Started(), rec.Flushed)
	}
}

// hijacker is a ResponseWriter whose connection can be taken over.
type hijacker struct {
	http.ResponseWriter
	conn net.Conn
}

func (h *hijacker) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return h.conn, bufio.NewReadWriter(bufio.NewReader(h.conn), bufio.NewWriter(h.conn)), nil
}

// http.ResponseController, which httputil.ReverseProxy uses for upgrades,
// reaches the wrapped writer through Unwrap.
func TestHijackThroughResponseController(t *testing.T) {
	server, client := net.Pipe()
	defer func() { _ = server.Close(); _ = client.Close() }()

	w := &TrackingWriter{ResponseWriter: &hijacker{httptest.NewRecorder(), server}}
	conn, _, err := http.NewResponseController(w).Hijack()
	if err != nil {
		t.Fatal(err)
	}
	if conn != server {
		t.Errorf("conn = %v", conn)
	}

	w = &TrackingWriter{ResponseWriter: httptest.NewRecorder()}
	if _, _, err := http.NewResponseController(w).Hijack(); !errors.Is(err, http.ErrNotSupported) {
		t.Errorf("Hijack = %v, want ErrNotSupported", err)
	}
}

// gorilla/websocket type-asserts http.Hijacker instead of unwrapping, so the
// method must be on TrackingWriter itself.
func TestHijackByTypeAssertion(t *testing.T) {
	server, client := net.Pipe()
	defer func() { _ = server.Close(); _ = client.Close() }()

	var w http.ResponseWriter = &TrackingWriter{ResponseWriter: &hijacker{httptest.NewRecorder(), server}}
	h, ok := w.(http.Hijacker)
	if !ok {
		t.Fatal("TrackingWriter does not implement http.Hijacker")
	}
	conn, _, err := h.Hijack()
	if err != nil || conn != server {
		t.Fatalf("Hijack = %v, %v", conn, err)
	}
	if tw := w.(*TrackingWriter); !tw.Started() || tw.Status() != http.StatusSwitchingProtocols {
		t.Errorf("Started() = %v, Status() = %d after a hijack; want true, 101", tw.Started(), tw.Status())
	}

	tw := &TrackingWriter{ResponseWriter: httptest.NewRecorder()}
	if _, _, err := tw.Hijack(); !errors.Is(err, http.ErrNotSupported) || tw.Started() {
		t.Errorf("Hijack = %v, started = %v; want ErrNotSupported, not started", err, tw.Started())
	}
}

func TestUnwrap(t *testing.T) {
	rec := httptest.NewRecorder()
	w := &TrackingWriter{ResponseWriter: rec}
	if w.Unwrap() != rec {
		t.Error("Unwrap did not return the wrapped writer")
	}
}

func TestBeforeStart(t *testing.T) {
	starts := map[string]func(w *TrackingWriter){
		"WriteHeader": func(w *TrackingWriter) { w.WriteHeader(http.StatusOK) },
		"Write":       func(w *TrackingWriter) { _, _ = w.Write([]byte("hi")) },
		"Flush":       func(w *TrackingWriter) { w.Flush() },
	}
	for name, start := range starts {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			calls := 0
			w := &TrackingWriter{ResponseWriter: rec}
			w.BeforeStart = func() {
				calls++
				w.Header().Set("X-Before", "yes")
			}
			start(w)
			_, _ = w.Write([]byte("more"))
			if calls != 1 {
				t.Errorf("BeforeStart ran %d times, want 1", calls)
			}
			if rec.Result().Header.Get("X-Before") != "yes" {
				t.Error("a header set in BeforeStart didn't go out")
			}
		})
	}
}

func TestBeforeStartSkipsInformational(t *testing.T) {
	ran := false
	w := &TrackingWriter{ResponseWriter: httptest.NewRecorder(), BeforeStart: func() { ran = true }}
	w.WriteHeader(http.StatusEarlyHints)
	if ran {
		t.Error("BeforeStart ran for a 1xx")
	}
}

func TestIsLocalPath(t *testing.T) {
	for p, want := range map[string]bool{
		"/":                 true,
		"/app/page?x=1#top": true,
		"":                  false,
		"app":               false,
		"//evil.com":        false,
		"/\\evil.com":       false,
		"/\t/evil.com":      false,
		"/\n/evil.com":      false,
		"https://evil.com":  false,
	} {
		if got := IsLocalPath(p); got != want {
			t.Errorf("IsLocalPath(%q) = %v, want %v", p, got, want)
		}
	}
}
