package viteserver

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
)

func TestProxyDevServer(t *testing.T) {
	vite := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		_, _ = rw.Write([]byte("vite"))
	}))
	defer vite.Close()

	u, err := url.Parse(vite.URL)
	if err != nil {
		t.Fatal(err)
	}

	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.Handle("/api/", http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		http.Error(rw, "backend", http.StatusTeapot)
	}))
	mux.Handle("/", ProxyDevServer(port))
	h := mux

	cases := []struct {
		method, target string
		wantCode       int
		wantBody       string
	}{
		{http.MethodGet, "/", http.StatusOK, "vite"},
		{http.MethodGet, "/src/main.ts", http.StatusOK, "vite"},
		{http.MethodGet, "/apiary", http.StatusOK, "vite"},
		{http.MethodGet, "/api/users", http.StatusTeapot, "backend\n"},
	}

	for _, c := range cases {
		rw := httptest.NewRecorder()
		h.ServeHTTP(rw, httptest.NewRequest(c.method, c.target, nil))

		if rw.Code != c.wantCode || rw.Body.String() != c.wantBody {
			t.Errorf("%s %s: got %d %q, want %d %q", c.method, c.target, rw.Code, rw.Body.String(), c.wantCode, c.wantBody)
		}
	}
}

func TestProxyDevServerForwards(t *testing.T) {
	vite := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		_, _ = rw.Write([]byte("vite " + req.URL.Path))
	}))
	defer vite.Close()

	u, err := url.Parse(vite.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}

	rw := httptest.NewRecorder()
	ProxyDevServer(port).ServeHTTP(rw, httptest.NewRequest(http.MethodGet, "/src/main.ts", nil))

	if rw.Code != http.StatusOK || rw.Body.String() != "vite /src/main.ts" {
		t.Errorf("got %d %q, want 200 %q", rw.Code, rw.Body.String(), "vite /src/main.ts")
	}
}
