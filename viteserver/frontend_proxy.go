package viteserver

import (
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
)

// ProxyDevServer forwards requests to the Vite dev server on
// localhost:vitePort, in the same container, so the app gets hot reload
// during development. Vite answers unknown paths with its own fallback, so
// test them against a MustAssets build before shipping.
func ProxyDevServer(vitePort int) http.Handler {
	target := &url.URL{
		Scheme: "http",
		Host:   net.JoinHostPort("localhost", strconv.Itoa(vitePort)),
	}

	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.SetXForwarded()
		},
		ErrorHandler: func(rw http.ResponseWriter, req *http.Request, err error) {
			slog.ErrorContext(req.Context(), "viteserver: vite dev server unreachable",
				"target", target.Host,
				"method", req.Method,
				"path", req.URL.Path,
				"err", err,
			)
			http.Error(rw, fmt.Sprintf("Vite dev server is not reachable on %s, is `vite` running?", target.Host), http.StatusBadGateway)
		},
	}

	return proxy
}
