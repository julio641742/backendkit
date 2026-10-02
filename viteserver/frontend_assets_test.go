package viteserver

import (
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func testFS() fstest.MapFS {
	return fstest.MapFS{
		"dist/index.html":           {Data: []byte("<html>index</html>")},
		"dist/favicon.ico":          {Data: []byte("ico")},
		"dist/assets/app-abc.js":    {Data: []byte("console.log('plain')")},
		"dist/assets/app-abc.js.br": {Data: []byte("BR")},
		"dist/assets/app-abc.js.gz": {Data: []byte("GZIP")},
		"dist/assets/lone.css.gz":   {Data: []byte("lone")},
		"other/secret.txt":          {Data: []byte("secret")},
	}
}

func do(t *testing.T, method, target string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()

	h := MustAssets(testFS(), "/dist/")

	req := httptest.NewRequest(method, target, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)

	return rw
}

func TestPrecompressedNegotiation(t *testing.T) {
	cases := []struct {
		accept, wantEncoding, wantBody string
	}{
		{"gzip, deflate, br", "br", "BR"},
		{"gzip", "gzip", "GZIP"},
		{"br;q=0, gzip", "gzip", "GZIP"},
		{"", "", "console.log('plain')"},
		{"identity", "", "console.log('plain')"},
		{"*", "br", "BR"},
	}

	for _, c := range cases {
		rw := do(t, http.MethodGet, "/assets/app-abc.js", map[string]string{"Accept-Encoding": c.accept})

		if rw.Code != http.StatusOK || rw.Body.String() != c.wantBody {
			t.Errorf("accept %q: got %d %q, want body %q", c.accept, rw.Code, rw.Body.String(), c.wantBody)
		}
		if got := rw.Header().Get("Content-Encoding"); got != c.wantEncoding {
			t.Errorf("accept %q: Content-Encoding %q, want %q", c.accept, got, c.wantEncoding)
		}
		if got := rw.Header().Get("Content-Type"); got != "text/javascript; charset=utf-8" {
			t.Errorf("accept %q: Content-Type %q", c.accept, got)
		}
		if got := rw.Header().Get("Cache-Control"); got != cacheImmutable {
			t.Errorf("accept %q: Cache-Control %q", c.accept, got)
		}
		if got := rw.Header().Get("Vary"); got != "Accept-Encoding" {
			t.Errorf("accept %q: Vary %q", c.accept, got)
		}
	}
}

func TestConditionalRequest(t *testing.T) {
	first := do(t, http.MethodGet, "/assets/app-abc.js", map[string]string{"Accept-Encoding": "br"})
	etag := first.Header().Get("ETag")

	rw := do(t, http.MethodGet, "/assets/app-abc.js", map[string]string{"Accept-Encoding": "br", "If-None-Match": etag})
	if rw.Code != http.StatusNotModified {
		t.Fatalf("got %d, want 304", rw.Code)
	}
	if rw.Header().Get("ETag") != etag || rw.Header().Get("Cache-Control") == "" {
		t.Errorf("304 missing validators: %v", rw.Header())
	}

	// The gzip variant has a different ETag, so it must not match the br one.
	rw = do(t, http.MethodGet, "/assets/app-abc.js", map[string]string{"Accept-Encoding": "gzip", "If-None-Match": etag})
	if rw.Code != http.StatusOK {
		t.Errorf("gzip with br etag: got %d, want 200", rw.Code)
	}
}

func TestRoutingAndFallback(t *testing.T) {
	cases := []struct {
		method, target, accept string
		wantCode               int
		wantBody, wantCache    string
	}{
		{http.MethodGet, "/", "", http.StatusOK, "<html>index</html>", cacheRevalidate},
		{http.MethodGet, "/users/42", "", http.StatusOK, "<html>index</html>", cacheRevalidate},
		{http.MethodGet, "/users/john.doe", "text/html", http.StatusOK, "<html>index</html>", cacheRevalidate},
		{http.MethodGet, "/favicon.ico", "", http.StatusOK, "ico", cacheRevalidate},
		{http.MethodGet, "/assets/missing.js", "text/html", http.StatusNotFound, "", "no-store"},
		{http.MethodGet, "/missing.json", "application/json", http.StatusNotFound, "", "no-store"},
		{http.MethodGet, "/../other/secret.txt", "", http.StatusNotFound, "", "no-store"},
		{http.MethodGet, "/assets/lone.css.gz", "", http.StatusOK, "lone", cacheImmutable},
		{http.MethodHead, "/", "", http.StatusOK, "", cacheRevalidate},
	}

	for _, c := range cases {
		rw := do(t, c.method, c.target, map[string]string{"Accept": c.accept})

		if rw.Code == http.StatusNotFound && strings.HasPrefix(rw.Header().Get("Content-Type"), "application/json") {
			rw.Body.Reset() // the httperr envelope
		}
		if rw.Code != c.wantCode || rw.Body.String() != c.wantBody {
			t.Errorf("%s %s: got %d %q, want %d %q", c.method, c.target, rw.Code, rw.Body.String(), c.wantCode, c.wantBody)
		}
		if got := rw.Header().Get("Cache-Control"); got != c.wantCache {
			t.Errorf("%s %s: Cache-Control %q, want %q", c.method, c.target, got, c.wantCache)
		}
	}
}

func TestRangeOnEncodedVariant(t *testing.T) {
	rw := do(t, http.MethodGet, "/assets/app-abc.js", map[string]string{"Accept-Encoding": "gzip", "Range": "bytes=0-1"})

	if rw.Code != http.StatusPartialContent || rw.Body.String() != "GZ" {
		t.Fatalf("got %d %q", rw.Code, rw.Body.String())
	}
	if cl := rw.Header().Get("Content-Length"); cl != "" && cl != "2" {
		t.Errorf("Content-Length %q for 2-byte range", cl)
	}
}

func TestMustAssetsPanics(t *testing.T) {
	tests := []struct {
		name   string
		fsys   fs.FS
		prefix string
		want   string
	}{
		{"missing index.html", testFS(), "/other/", "index.html not found"},
		{"missing prefix", testFS(), "/nope/", ""},
		{"invalid prefix", testFS(), "/../dist", ""},
		{"files can't seek", noSeekFS{testFS()}, "/dist/", "can't seek"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				msg, _ := recover().(string)
				if !strings.HasPrefix(msg, "viteserver: ") || !strings.Contains(msg, tt.want) {
					t.Errorf("panic = %q, want one with the package prefix and %q", msg, tt.want)
				}
			}()
			MustAssets(tt.fsys, tt.prefix)
		})
	}
}

// noSeekFS hides the Seek method of its regular files.
type noSeekFS struct{ fsys fstest.MapFS }

func (n noSeekFS) Open(name string) (fs.File, error) {
	f, err := n.fsys.Open(name)
	if err != nil {
		return nil, err
	}
	if fi, err := f.Stat(); err != nil || fi.IsDir() {
		return f, err
	}
	return struct{ fs.File }{f}, nil
}

// brokenFS opens files for the startup scan, then fails. It doesn't embed
// MapFS, whose Sub would bypass Open.
type brokenFS struct {
	fsys   fstest.MapFS
	broken bool
}

func (b *brokenFS) Open(name string) (fs.File, error) {
	if b.broken {
		return nil, errors.New("disk gone")
	}
	return b.fsys.Open(name)
}

func TestServeErrorUsesEnvelope(t *testing.T) {
	fsys := &brokenFS{fsys: testFS()}
	h := MustAssets(fsys, "/dist/")
	fsys.broken = true

	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, httptest.NewRequest(http.MethodGet, "/", nil))

	if rw.Code != http.StatusInternalServerError {
		t.Fatalf("code = %d, want 500", rw.Code)
	}
	if ct := rw.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want the JSON envelope", ct)
	}
	// The asset's long-lived Cache-Control gives way to the envelope's no-store.
	if rw.Header().Get("ETag") != "" || rw.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("asset headers left on the error: %v", rw.Header())
	}
}

// Mounted on "/" as the package doc shows, backend routes outside /api
// (the OAuth callback, health checks) reach the backend instead of getting
// the SPA's index.html.
func TestMountedOnMux(t *testing.T) {
	backend := http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		http.Error(rw, "backend", http.StatusTeapot)
	})
	mux := http.NewServeMux()
	mux.Handle("/api/", backend)
	mux.Handle("GET /oauth/callback", backend)
	mux.Handle("GET /healthz", backend)
	mux.Handle("/", MustAssets(testFS(), "/dist/"))

	cases := []struct {
		method, target string
		wantCode       int
		wantBody       string
	}{
		{http.MethodGet, "/oauth/callback", http.StatusTeapot, "backend\n"},
		{http.MethodGet, "/healthz", http.StatusTeapot, "backend\n"},
		{http.MethodGet, "/api/unknown", http.StatusTeapot, "backend\n"},
		{http.MethodPost, "/api/users", http.StatusTeapot, "backend\n"},
		{http.MethodGet, "/users/42", http.StatusOK, "<html>index</html>"},
		{http.MethodGet, "/apiary", http.StatusOK, "<html>index</html>"},
	}
	for _, c := range cases {
		rw := httptest.NewRecorder()
		mux.ServeHTTP(rw, httptest.NewRequest(c.method, c.target, nil))
		if rw.Code != c.wantCode || rw.Body.String() != c.wantBody {
			t.Errorf("%s %s: got %d %q, want %d %q", c.method, c.target, rw.Code, rw.Body.String(), c.wantCode, c.wantBody)
		}
	}

	// Other methods never get index.html.
	rw := httptest.NewRecorder()
	mux.ServeHTTP(rw, httptest.NewRequest(http.MethodPost, "/users/42", nil))
	if rw.Code != http.StatusMethodNotAllowed || rw.Header().Get("Allow") != "GET, HEAD" {
		t.Errorf("POST /users/42: got %d, Allow %q", rw.Code, rw.Header().Get("Allow"))
	}
}
