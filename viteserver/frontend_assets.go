// Package viteserver serves the Vue frontend: the compiled, precompressed Vite
// build in production, and the Vite dev server in development. Both are plain
// handlers to mount on "/", where http.ServeMux routes them every request
// that no more specific pattern claims:
//
//	mux.Handle("/api/", api)
//	mux.Handle("GET /oauth/callback", callback)
//	mux.Handle("/", viteserver.MustAssets(dist, "dist")) // or viteserver.ProxyDevServer(5173)
//
// Unknown paths without an extension are answered with index.html, so give
// the API a catch-all ("/api/" above) for its unknown routes to 404 rather
// than reach the frontend.
package viteserver

import (
	"fmt"
	"hash/fnv"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/julio641742/backendkit/httperr"
)

// Vite emits content-hashed files under build.assetsDir, which default to "assets".
const assetsDir = "assets/"

const (
	cacheImmutable   = "public, max-age=31536000, immutable"
	cacheRevalidate  = "no-cache"
	defaultIndexFile = "index.html"
)

// encodings lists the supported precompressed variants in server preference order.
var encodings = []struct{ name, ext string }{
	{"br", ".br"},
	{"zstd", ".zst"},
	{"gzip", ".gz"},
}

type variant struct {
	path     string // path inside the served fs
	etag     string // quoted strong ETag
	encoding string // "" for the identity representation
}

type asset struct {
	contentType  string
	cacheControl string
	identity     variant
	encoded      map[string]variant // keyed by encoding name
}

type assetServer struct {
	fs     fs.FS
	assets map[string]*asset // keyed by clean path relative to the fs root, e.g. "assets/app-1a2b.js"
}

// MustAssets serves the Vite build found under fsPrefix in fsys, typically an
// embed.FS, picking the best precompressed variant (.br, .zst, .gz) of each
// file the client accepts, and the file itself otherwise. A missing path is
// answered with index.html when it has no extension or the request accepts
// text/html, and with a 404 otherwise. Methods other than GET and HEAD get a
// 405. It panics when fsPrefix can't be opened, a file can't be read or seek
// (those of embed.FS and os.DirFS can), or there is no index.html: with an
// embedded build, that is a build mistake.
//
// Content-Type comes from mime.TypeByExtension (except .js and .mjs), whose
// built-in table is small: .woff2, .woff, .ttf, .ico, .webmanifest and .txt
// are only known from the host's MIME database (/etc/mime.types and the
// like) and are otherwise served as application/octet-stream. Minimal images
// such as scratch or distroless have none, so install one (e.g. the
// media-types or mailcap package) or copy a mime.types file into the image.
func MustAssets(fsys fs.FS, fsPrefix string) http.Handler {
	s, err := newAssetServer(fsys, fsPrefix)
	if err != nil {
		panic(fmt.Sprintf("viteserver: %v", err))
	}

	return s
}

func newAssetServer(fsys fs.FS, fsPrefix string) (*assetServer, error) {
	root := strings.Trim(fsPrefix, "/")
	if root == "" {
		root = "."
	}

	sub, err := fs.Sub(fsys, root)
	if err != nil {
		return nil, err
	}

	s := &assetServer{
		fs:     sub,
		assets: make(map[string]*asset),
	}

	if err = s.hashFiles(); err != nil {
		return nil, err
	}

	if _, ok := s.assets[defaultIndexFile]; !ok {
		return nil, fmt.Errorf("%s not found in %q", defaultIndexFile, root)
	}

	return s, nil
}

func (s *assetServer) hashFiles() error {
	var files []string
	exists := make(map[string]bool)

	err := fs.WalkDir(s.fs, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}

		files = append(files, p)
		exists[p] = true

		return nil
	})
	if err != nil {
		return err
	}

	// A compressed file is only treated as a variant when its original exists,
	// so the original is registered first and variants attached afterwards.
	var compressed []string

	for _, p := range files {
		if _, enc := originalOf(p, exists); enc != "" {
			compressed = append(compressed, p)
			continue
		}

		v, err := s.hashFile(p, "")
		if err != nil {
			return err
		}

		s.assets[p] = &asset{
			contentType:  contentTypeOf(p),
			cacheControl: cacheControlOf(p),
			identity:     v,
			encoded:      make(map[string]variant),
		}
	}

	for _, p := range compressed {
		original, enc := originalOf(p, exists)

		v, err := s.hashFile(p, enc)
		if err != nil {
			return err
		}

		s.assets[original].encoded[enc] = v
	}

	return nil
}

// originalOf reports the uncompressed file and encoding name for a precompressed
// file, or "" if p is not a variant of an existing file.
func originalOf(p string, exists map[string]bool) (string, string) {
	for _, e := range encodings {
		if original, ok := strings.CutSuffix(p, e.ext); ok && exists[original] {
			return original, e.name
		}
	}

	return "", ""
}

func (s *assetServer) hashFile(p, encoding string) (variant, error) {
	fd, err := s.fs.Open(p)
	if err != nil {
		return variant{}, err
	}
	defer func() { _ = fd.Close() }()

	// serveAsset hands files to http.ServeContent, which needs to seek. The
	// files of embed.FS and os.DirFS can.
	if _, ok := fd.(io.ReadSeeker); !ok {
		return variant{}, fmt.Errorf("%s: file can't seek", p)
	}

	hasher := fnv.New64a()

	if _, err := io.Copy(hasher, fd); err != nil {
		return variant{}, err
	}

	return variant{
		path:     p,
		etag:     `"` + strconv.FormatUint(hasher.Sum64(), 36) + `"`,
		encoding: encoding,
	}, nil
}

func contentTypeOf(p string) string {
	switch ext := path.Ext(p); ext {
	case ".js", ".mjs":
		// Some hosts (notably the Windows registry) map .js to text/plain, which breaks module scripts.
		return "text/javascript; charset=utf-8"
	default:
		if contentType := mime.TypeByExtension(ext); contentType != "" {
			return contentType
		}

		return "application/octet-stream"
	}
}

func cacheControlOf(p string) string {
	if strings.HasPrefix(p, assetsDir) {
		return cacheImmutable
	}

	// index.html and public/ files keep stable names, so always revalidate them via ETag.
	return cacheRevalidate
}

func (s *assetServer) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		rw.Header().Set("Allow", "GET, HEAD")
		httperr.Write(rw, http.StatusMethodNotAllowed, "")
		return
	}

	name := strings.TrimPrefix(path.Clean("/"+req.URL.Path), "/")
	if name == "" {
		name = defaultIndexFile
	}

	a, found := s.assets[name]
	if !found {
		if !isSPARoute(req, name) {
			httperr.Write(rw, http.StatusNotFound, "")
			return
		}

		a = s.assets[defaultIndexFile]
	}

	s.serveAsset(rw, req, a)
}

// isSPARoute reports whether a missing file should fall back to index.html.
func isSPARoute(req *http.Request, name string) bool {
	if strings.HasPrefix(name, assetsDir) {
		return false
	}

	return path.Ext(name) == "" || strings.Contains(req.Header.Get("Accept"), "text/html")
}

func (s *assetServer) serveAsset(rw http.ResponseWriter, req *http.Request, a *asset) {
	h := rw.Header()

	v := a.identity
	if len(a.encoded) > 0 {
		h.Add("Vary", "Accept-Encoding")

		if enc, ok := negotiateEncoding(req.Header.Get("Accept-Encoding"), a.encoded); ok {
			v = enc
		}
	}

	h.Set("Content-Type", a.contentType)
	h.Set("Cache-Control", a.cacheControl)
	h.Set("ETag", v.etag)
	h.Set("X-Content-Type-Options", "nosniff")

	if v.encoding != "" {
		h.Set("Content-Encoding", v.encoding)
	}

	fd, err := s.fs.Open(v.path)
	if err != nil {
		s.serveError(rw, req, v.path, err)
		return
	}
	defer func() { _ = fd.Close() }()

	// ServeContent handles If-None-Match against the ETag above, HEAD, and
	// Range. hashFile checked at startup that the file can seek.
	http.ServeContent(rw, req, v.path, time.Time{}, fd.(io.ReadSeeker))
}

// serveError answers a file that was found at startup but can't be read now
// with the same JSON 500 envelope as the rest of the app.
func (s *assetServer) serveError(rw http.ResponseWriter, req *http.Request, p string, err error) {
	slog.ErrorContext(req.Context(), "viteserver: unable to serve asset", "path", p, "err", err)

	// Drop the asset headers set for the success path.
	h := rw.Header()
	for _, k := range []string{"Cache-Control", "Content-Encoding", "ETag", "Vary"} {
		h.Del(k)
	}

	httperr.Write(rw, http.StatusInternalServerError, "")
}

// negotiateEncoding picks the first variant, in server preference order, that
// the Accept-Encoding header allows (honouring q=0 and "*").
func negotiateEncoding(header string, available map[string]variant) (variant, bool) {
	if header == "" {
		return variant{}, false
	}

	accepted := make(map[string]float64)

	for part := range strings.SplitSeq(header, ",") {
		name, params, _ := strings.Cut(part, ";")
		name = strings.ToLower(strings.TrimSpace(name))
		q := 1.0

		for param := range strings.SplitSeq(params, ";") {
			if v, ok := strings.CutPrefix(strings.TrimSpace(param), "q="); ok {
				if parsed, err := strconv.ParseFloat(v, 64); err == nil {
					q = parsed
				}
			}
		}

		accepted[name] = q
	}

	for _, e := range encodings {
		v, ok := available[e.name]
		if !ok {
			continue
		}

		q, listed := accepted[e.name]
		if !listed {
			q, listed = accepted["*"]
		}

		if listed && q > 0 {
			return v, true
		}
	}

	return variant{}, false
}
