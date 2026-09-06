package authz

// Client source resolution (spec section 12.3): the value every per-source
// limit keys on and the `source` field of every audit record.
//
// The rules, in the order they bite:
//
//  1. With AGENT_GM_TRUSTED_PROXY_CIDRS unset or empty, forwarded headers are
//     ignored ENTIRELY. Not "preferred less"; not read at all.
//  2. With a CIDR list set, and only if the TCP peer is inside it, the source
//     is the RIGHTMOST X-Forwarded-For entry that is not itself inside the
//     list. Otherwise the TCP peer.
//  3. The walk is right to left. Taking the leftmost entry is the usual bug
//     and would turn every per-source limit in the system into decoration,
//     because the leftmost entry is the one the client chooses.
//  4. An unparseable entry STOPS the walk and falls back to the TCP peer. It
//     is not skipped: a header we cannot fully parse is a header we cannot
//     reason about, and skipping would let an attacker hide a hop.
//  5. An invalid CIDR list refuses to start. An operator who mistypes the list
//     should find out immediately, not discover months later that the trust
//     they configured was never in force.
//  6. The resolved source is never the empty string.
//  7. X-Forwarded-Proto and X-Forwarded-Host are recorded but never build a
//     URL. Every URL comes from AGENT_GM_PUBLIC_URL alone.

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// Startup log values for client_source_mode (spec section 12.3).
const (
	SourceModeSocketPeer   = "socket_peer"
	SourceModeTrustedProxy = "trusted_proxy"
)

// SourceUnknown is the source of a request whose TCP peer could not be parsed
// at all. It exists so that rule 6 -- the resolved source is never the empty
// string -- holds even for a malformed RemoteAddr, and so that such requests
// share one bucket rather than each getting their own.
const SourceUnknown = "unknown"

// Source is one resolved client source.
type Source struct {
	// Value is the source itself: an IP address, or SourceUnknown. Never
	// empty.
	Value string
	// Mode is SourceModeSocketPeer or SourceModeTrustedProxy: which rule
	// produced Value. GET /v1/health reports it, so a misconfiguration where
	// every caller collapses to one source is visible.
	Mode string
	// ForwardedProto and ForwardedHost are recorded for diagnosis and NEVER
	// used to build a URL.
	ForwardedProto string
	ForwardedHost  string
}

// SourceResolver resolves a request's client source. The zero value is not
// usable; construct it with NewSourceResolver.
type SourceResolver struct {
	trusted []netip.Prefix
}

// NewSourceResolver parses AGENT_GM_TRUSTED_PROXY_CIDRS.
//
// The list is comma-separated. An entry may be a CIDR (`10.0.0.0/8`,
// `::1/128`) or a bare address, which is read as a host route. An invalid
// entry is an error, and the caller is expected to refuse to start on it.
func NewSourceResolver(cidrList string) (*SourceResolver, error) {
	r := &SourceResolver{}
	for _, raw := range strings.Split(cidrList, ",") {
		entry := strings.TrimSpace(raw)
		if entry == "" {
			continue
		}
		p, err := parsePrefix(entry)
		if err != nil {
			return nil, fmt.Errorf("AGENT_GM_TRUSTED_PROXY_CIDRS: %q is not a CIDR or an IP address: %w", entry, err)
		}
		r.trusted = append(r.trusted, p)
	}
	return r, nil
}

// parsePrefix reads a CIDR, or a bare address as a host route.
func parsePrefix(entry string) (netip.Prefix, error) {
	if strings.Contains(entry, "/") {
		p, err := netip.ParsePrefix(entry)
		if err != nil {
			return netip.Prefix{}, err
		}
		// Unmap so that an IPv4-mapped IPv6 prefix and an IPv4 prefix compare
		// alike; Masked() normalises host bits so 10.1.2.3/8 is not silently a
		// different prefix from 10.0.0.0/8.
		return netip.PrefixFrom(p.Addr().Unmap(), p.Bits()).Masked(), nil
	}
	a, err := netip.ParseAddr(entry)
	if err != nil {
		return netip.Prefix{}, err
	}
	a = a.Unmap()
	return netip.PrefixFrom(a, a.BitLen()), nil
}

// Trusts reports whether the resolver has any trusted proxies configured.
func (r *SourceResolver) Trusts() bool { return len(r.trusted) > 0 }

// Mode is the startup value of client_source_mode.
func (r *SourceResolver) Mode() string {
	if r.Trusts() {
		return SourceModeTrustedProxy
	}
	return SourceModeSocketPeer
}

// contains reports whether addr is inside the trusted list. An IPv4-mapped
// IPv6 address (`::ffff:127.0.0.1`) is unmapped first, so it matches an IPv4
// CIDR -- which is what a Go listener hands you on a dual-stack socket, and
// what an operator who wrote `127.0.0.1/32` plainly meant.
func (r *SourceResolver) contains(addr netip.Addr) bool {
	a := addr.Unmap()
	for _, p := range r.trusted {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// Resolve resolves the source of one request from its TCP peer
// (http.Request.RemoteAddr) and its headers.
func (r *SourceResolver) Resolve(remoteAddr string, headers http.Header) Source {
	src := Source{
		Value: SourceUnknown,
		Mode:  r.Mode(),
	}
	if headers != nil {
		src.ForwardedProto = headers.Get("X-Forwarded-Proto")
		src.ForwardedHost = headers.Get("X-Forwarded-Host")
	}

	peer, peerOK := parsePeer(remoteAddr)
	if peerOK {
		src.Value = peer.String()
	}

	// Rule 1: no trusted proxies, forwarded headers ignored entirely.
	if !r.Trusts() {
		return src
	}
	// Rule 2: the peer must itself be trusted before any header is believed.
	if !peerOK || !r.contains(peer) {
		return src
	}

	entries := forwardedFor(headers)
	// Rules 3 and 4: right to left; an unparseable entry stops the walk.
	for i := len(entries) - 1; i >= 0; i-- {
		hop, err := parseForwardedEntry(entries[i])
		if err != nil {
			return src // fall back to the TCP peer
		}
		if r.contains(hop) {
			continue
		}
		src.Value = hop.String()
		return src
	}
	// Every entry was trusted, or there were none: the peer stands.
	return src
}

// parsePeer reads http.Request.RemoteAddr, which is host:port for a TCP
// listener but is occasionally a bare address in tests and on other
// transports.
func parsePeer(remoteAddr string) (netip.Addr, bool) {
	s := strings.TrimSpace(remoteAddr)
	if s == "" {
		return netip.Addr{}, false
	}
	if host, _, err := net.SplitHostPort(s); err == nil {
		s = host
	}
	// A bracketed IPv6 literal without a port.
	s = strings.TrimPrefix(strings.TrimSuffix(s, "]"), "[")
	// A zone (fe80::1%eth0) is not part of the identity for limiting.
	if i := strings.IndexByte(s, '%'); i >= 0 {
		s = s[:i]
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Addr{}, false
	}
	return a.Unmap(), true
}

// forwardedFor flattens every X-Forwarded-For header, in order, into one list
// of entries. Several headers are equivalent to one comma-joined header, which
// is what RFC 7230 says of any comma-separated list header, and reading only
// the first would let a proxy chain hide hops by splitting.
func forwardedFor(headers http.Header) []string {
	if headers == nil {
		return nil
	}
	var out []string
	for _, h := range headers.Values("X-Forwarded-For") {
		for _, part := range strings.Split(h, ",") {
			out = append(out, strings.TrimSpace(part))
		}
	}
	return out
}

// parseForwardedEntry parses one X-Forwarded-For entry. An entry with a port,
// which some proxies emit, is accepted; anything else is an error and stops
// the walk.
func parseForwardedEntry(entry string) (netip.Addr, error) {
	s := strings.TrimSpace(entry)
	if s == "" {
		return netip.Addr{}, fmt.Errorf("empty X-Forwarded-For entry")
	}
	if a, err := netip.ParseAddr(strings.Trim(s, "[]")); err == nil {
		return a.Unmap(), nil
	}
	if host, _, err := net.SplitHostPort(s); err == nil {
		if a, err := netip.ParseAddr(host); err == nil {
			return a.Unmap(), nil
		}
	}
	return netip.Addr{}, fmt.Errorf("unparseable X-Forwarded-For entry %q", entry)
}
