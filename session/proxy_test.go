package session

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

func TestStripProxyRequest(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	// HTTP/2 clients may split cookies over several headers.
	req.Header.Add("Cookie", serviceCookieName+"=s; theme=dark")
	req.Header.Add("Cookie", stateCookiePrefix+"S=t; lang=en")

	StripProxyRequest(req)

	if got, want := req.Header.Values("Cookie"), []string{"theme=dark; lang=en"}; !slices.Equal(got, want) {
		t.Errorf("Cookie = %q, want %q", got, want)
	}

	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Cookie", serviceCookieName+"=s")
	StripProxyRequest(req)
	if _, ok := req.Header["Cookie"]; ok {
		t.Errorf("Cookie = %q, want no header", req.Header.Values("Cookie"))
	}
}

// The upstream's own XSRF-TOKEN cookie and header are its CSRF protection
// (Laravel, Angular and Axios use those names): stripping them made every
// write to such an upstream fail.
func TestStripProxyKeepsUpstreamCSRF(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("Cookie", csrfCookieName+"=t; "+serviceCookieName+"=s")
	req.Header.Set(csrfHeaderName, "t")
	StripProxyRequest(req)
	if got, want := req.Header.Get("Cookie"), csrfCookieName+"=t"; got != want {
		t.Errorf("Cookie = %q, want %q", got, want)
	}
	if got := req.Header.Get(csrfHeaderName); got != "t" {
		t.Errorf("%s = %q, want t", csrfHeaderName, got)
	}

	resp := &http.Response{Header: http.Header{"Set-Cookie": {csrfCookieName + "=t; Path=/"}}}
	if err := StripProxyResponse(resp); err != nil {
		t.Fatal(err)
	}
	if got, want := resp.Header.Values("Set-Cookie"), []string{csrfCookieName + "=t; Path=/"}; !slices.Equal(got, want) {
		t.Errorf("Set-Cookie = %q, want %q", got, want)
	}
}

func TestStripProxyResponse(t *testing.T) {
	resp := &http.Response{Header: http.Header{"Set-Cookie": {
		serviceCookieName + "=x; Secure",
		"theme=dark; Path=/",
		stateCookiePrefix + "S=; Max-Age=0",
		"not a cookie",
	}}}

	if err := StripProxyResponse(resp); err != nil {
		t.Fatal(err)
	}
	if got, want := resp.Header.Values("Set-Cookie"), []string{"theme=dark; Path=/"}; !slices.Equal(got, want) {
		t.Errorf("Set-Cookie = %q, want %q", got, want)
	}
}

// Cookies Go can't parse, or would quote when re-serializing, still reach
// the upstream byte for byte: they belong to the proxied app.
func TestStripProxyRequestKeepsOtherCookiesVerbatim(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Cookie", `prefs={"a":1}; `+serviceCookieName+`=s; q=a b; name=café;`+stateCookiePrefix+`S=t`)

	StripProxyRequest(req)

	if got, want := req.Header.Values("Cookie"), []string{`prefs={"a":1}; q=a b; name=café`}; !slices.Equal(got, want) {
		t.Errorf("Cookie = %q, want %q", got, want)
	}
}

func TestStripProxyResponseKeepsOtherCookiesVerbatim(t *testing.T) {
	resp := &http.Response{Header: http.Header{"Set-Cookie": {
		`prefs={"a":1}; Path=/`,
		"name=café",
		" " + serviceCookieName + " =x",
		// Nameless: some browsers send it back as "__Host-svc=evil".
		"=" + serviceCookieName + "=evil",
		serviceCookieName,
	}}}

	if err := StripProxyResponse(resp); err != nil {
		t.Fatal(err)
	}
	if got, want := resp.Header.Values("Set-Cookie"), []string{`prefs={"a":1}; Path=/`, "name=café"}; !slices.Equal(got, want) {
		t.Errorf("Set-Cookie = %q, want %q", got, want)
	}
}
