package session

import (
	"net/http"
	"strings"
)

// StripProxyRequest removes the service host's cookies from a request a
// reverse proxy forwards upstream, so the upstream never sees a visitor's
// service session. The client's other cookies, and its X-XSRF-TOKEN header,
// are the upstream's own and are passed on verbatim, even those net/http
// can't parse. Call it on pr.Out in httputil.ReverseProxy.Rewrite.
func StripProxyRequest(out *http.Request) {
	var kept []string
	for _, line := range out.Header.Values("Cookie") {
		for pair := range strings.SplitSeq(line, ";") {
			if pair = strings.TrimSpace(pair); pair != "" && !isServiceCookie(cookiePairName(pair)) {
				kept = append(kept, pair)
			}
		}
	}
	out.Header.Del("Cookie")
	if len(kept) > 0 {
		out.Header.Set("Cookie", strings.Join(kept, "; "))
	}
}

// StripProxyResponse removes the upstream's Set-Cookie lines for the service
// host's cookies, so an upstream can neither set nor delete them. Nameless
// cookies are dropped too: some browsers send "=__Host-svc=x" back as
// "__Host-svc=x". It is an httputil.ReverseProxy ModifyResponse.
func StripProxyResponse(resp *http.Response) error {
	lines := resp.Header.Values("Set-Cookie")
	resp.Header.Del("Set-Cookie")
	for _, line := range lines {
		pair, _, _ := strings.Cut(line, ";")
		if name := cookiePairName(pair); name != "" && strings.Contains(pair, "=") && !isServiceCookie(name) {
			resp.Header.Add("Set-Cookie", line)
		}
	}
	return nil
}

// cookiePairName returns the name of a "name=value" pair, trimmed the way
// browsers trim it.
func cookiePairName(pair string) string {
	name, _, _ := strings.Cut(pair, "=")
	return strings.TrimSpace(name)
}

// isServiceCookie reports the cookies ServiceMiddleware sets. The portal's
// are host-only on another site, so they never reach a service host.
func isServiceCookie(name string) bool {
	return name == serviceCookieName || strings.HasPrefix(name, stateCookiePrefix)
}
