package oauth

// Redirect URI rules, per RFC 8252 and spec section 9.3.
//
// The table is short and every row of it is a refusal somebody would
// otherwise argue for:
//
//	Accepted                                   | Refused
//	-------------------------------------------|------------------------------
//	https:// with a fully qualified host        | http:// on any non-loopback host
//	http://127.0.0.1[:port]/...                 | http://localhost.evil.example/...
//	http://[::1][:port]/...                     | https:// with an IP literal
//	http://localhost[:port]/...                 | any URI with a fragment or embedded credentials
//	a private-use scheme containing a dot       |
//
// **The deliberate deviation, recorded as contract text.**
// `http://localhost/...` is accepted, against RFC 8252 section 8.3, which
// prefers the IP literals because `localhost` resolution depends on the
// host's name service. That hazard exists only on the client's own machine,
// and widely used MCP clients register the name; refusing them buys little.
// The allowance is for the literal host name and NOTHING ELSE: the comparison
// is exact and case-insensitive, so `localhost.evil.example`, `notlocalhost`,
// `local.host` and `localhost@evil.example` are ordinary domains with no
// plain-http exemption. A suffix or substring match here would be far worse
// than the problem the allowance solves -- which is why the loopback test
// below is `==` on a lowercased hostname and can never be anything else.

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// MaxRedirectURIs and MaxRedirectURILength are section 9.3's bounds.
const (
	MaxRedirectURIs      = 10
	MaxRedirectURILength = 500
)

// loopbackHosts is the exact, closed set. Matching is `==` on the lowercased
// hostname; there is no prefix, suffix or substring test anywhere in this
// file, and there must never be one.
var loopbackHosts = map[string]bool{
	"127.0.0.1": true,
	"::1":       true,
	"localhost": true,
}

// ValidateRedirectURI reports why a registration's redirect URI is refused,
// or nil if it is acceptable.
func ValidateRedirectURI(raw string) error {
	if len(raw) == 0 {
		return fmt.Errorf("a redirect URI may not be empty")
	}
	if len(raw) > MaxRedirectURILength {
		return fmt.Errorf("a redirect URI may be at most %d characters", MaxRedirectURILength)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%q is not a URI", raw)
	}
	// A fragment or embedded credentials, whatever the scheme. Both are
	// refused before anything else is considered: a URI carrying either is
	// not a redirect target this server will ever hand a code to.
	if u.Fragment != "" || strings.Contains(raw, "#") {
		return fmt.Errorf("a redirect URI may not carry a fragment")
	}
	if u.User != nil || strings.Contains(u.Host, "@") {
		return fmt.Errorf("a redirect URI may not carry embedded credentials")
	}

	scheme := strings.ToLower(u.Scheme)
	switch scheme {
	case "https":
		host := strings.ToLower(u.Hostname())
		if host == "" {
			return fmt.Errorf("an https redirect URI needs a host")
		}
		if net.ParseIP(host) != nil {
			return fmt.Errorf("an https redirect URI may not use an IP literal")
		}
		if !strings.Contains(host, ".") {
			return fmt.Errorf("an https redirect URI needs a fully qualified host")
		}
		return nil

	case "http":
		host := strings.ToLower(u.Hostname())
		if !loopbackHosts[host] {
			return fmt.Errorf("plain http is accepted only on 127.0.0.1, [::1] and localhost")
		}
		return nil

	case "":
		return fmt.Errorf("a redirect URI needs a scheme")

	default:
		// A private-use scheme, per RFC 8252 section 7.1: reverse-domain
		// form, so it must contain a dot. `myapp:/cb` is refused because a
		// scheme with no dot is not a name its owner can prove they hold.
		if !strings.Contains(scheme, ".") {
			return fmt.Errorf("a private-use scheme must contain a dot")
		}
		return nil
	}
}

// IsLoopbackRedirect reports whether a registered URI is one of the three
// loopback forms, which is what makes it port-agnostic at authorization time.
func IsLoopbackRedirect(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	if strings.ToLower(u.Scheme) != "http" {
		return false
	}
	return loopbackHosts[strings.ToLower(u.Hostname())]
}

// RedirectMatches reports whether a redirect URI presented at
// `/oauth/authorize` matches one that was registered.
//
// A registered loopback redirect matches ANY PORT (RFC 8252 section 7.3), for
// `127.0.0.1`, `[::1]` and `localhost` alike -- because a native client binds
// an ephemeral port it cannot know at registration time. Everything else about
// the URI must still be equal: scheme, host, path and query. So a registered
// `http://127.0.0.1/cb` matches `http://127.0.0.1:53211/cb`, and matches
// neither `http://127.0.0.1:53211/cb2` nor `http://127.0.0.2/cb`.
//
// Note what is NOT relaxed: the token endpoint still requires `redirect_uri`
// to equal the one bound to the code exactly (section 9.3), so the port a
// client authorized on is the port it must present at exchange.
func RedirectMatches(registered, presented string) bool {
	if registered == presented {
		return true
	}
	if !IsLoopbackRedirect(registered) {
		return false
	}
	reg, err := url.Parse(registered)
	if err != nil {
		return false
	}
	pres, err := url.Parse(presented)
	if err != nil {
		return false
	}
	if pres.User != nil || pres.Fragment != "" {
		return false
	}
	if !strings.EqualFold(pres.Scheme, reg.Scheme) {
		return false
	}
	if !strings.EqualFold(pres.Hostname(), reg.Hostname()) {
		return false
	}
	if pres.EscapedPath() != reg.EscapedPath() {
		return false
	}
	return pres.RawQuery == reg.RawQuery
}

// MatchRegisteredRedirect returns the registered URI that matches, or "" and
// false. The presented value is returned rather than the registered one where
// they differ only by port, because that is the URI the code is bound to and
// the one the token endpoint will require exactly.
func MatchRegisteredRedirect(registered []string, presented string) (string, bool) {
	for _, candidate := range registered {
		if RedirectMatches(candidate, presented) {
			return presented, true
		}
	}
	return "", false
}
