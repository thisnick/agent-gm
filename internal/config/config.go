// Package config reads Agent GM's environment. There is no config file
// (spec section 15.1), and secrets are read from nowhere but the environment.
package config

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/thisnick/agent-gm/internal/store"
)

// Backend selects the Google Messages implementation.
type Backend string

const (
	// BackendLibGM is the real thing.
	BackendLibGM Backend = "libgm"
	// BackendFake is the deterministic in-memory Google Messages. It
	// requires AGENT_GM_ALLOW_FAKE=1 as well, so a production deployment
	// cannot be talked into serving an empty in-memory phone.
	BackendFake Backend = "fake"
)

// Config is the environment Agent GM was started with.
type Config struct {
	ConnectionMode string
	// PublicURL is AGENT_GM_PUBLIC_URL: the issuer, the canonical resource,
	// and the base of every URL Agent GM hands out. It is NEVER derived from
	// the request's Host, so an agent in a sandbox on another machine
	// reaches the same origin the tunnel exposes (spec sections 10.3, 12.3).
	PublicURL string
	// ListenAddr is AGENT_GM_LISTEN_ADDR.
	ListenAddr string
	// AdminSecret is AGENT_GM_ADMIN_SECRET, the owner bootstrap credential.
	// It lives in the environment only and is at least 43 characters; the
	// server refuses to start with a shorter one (spec section 12.1).
	AdminSecret string
	// TrustedProxyCIDRs is AGENT_GM_TRUSTED_PROXY_CIDRS, parsed. An invalid
	// value refuses to start: an operator who mistypes the list should find
	// out immediately, not discover months later that the trust they
	// configured was never in force (spec section 12.3).
	TrustedProxyCIDRs []netip.Prefix
	// TrustedProxyRaw is the value as given, for the one startup log line
	// that reports which mode is in force.
	TrustedProxyRaw string

	DataDir        string
	DataKey        store.DataKey
	Backend        Backend
	LogLevel       string
	LogFormat      string
	UnsafeTrace    bool
	Chrome         string
	PairingTimeout time.Duration
	DefaultAccount string
	StateDir       string
}

// ErrFakeNotAllowed is spec section 15.1: AGENT_GM_BACKEND=fake without
// AGENT_GM_ALLOW_FAKE=1 refuses to start.
var ErrFakeNotAllowed = errors.New(
	"AGENT_GM_BACKEND=fake also requires AGENT_GM_ALLOW_FAKE=1; " +
		"refusing to serve an empty in-memory phone")

// Load reads the environment.
func Load() (Config, error) {
	var c Config

	c.DataDir = env("AGENT_GM_DATA_DIR", "/data")
	c.ConnectionMode = env("AGENT_GM_CONNECTION_MODE", "push")
	if c.ConnectionMode != "push" && c.ConnectionMode != "active" {
		return c, errors.New("AGENT_GM_CONNECTION_MODE must be push or active")
	}

	c.Backend = Backend(strings.ToLower(env("AGENT_GM_BACKEND", string(BackendLibGM))))
	switch c.Backend {
	case BackendLibGM:
	case BackendFake:
		if os.Getenv("AGENT_GM_ALLOW_FAKE") != "1" {
			return c, ErrFakeNotAllowed
		}
	default:
		return c, fmt.Errorf("AGENT_GM_BACKEND must be %q or %q, not %q",
			BackendLibGM, BackendFake, c.Backend)
	}

	raw := os.Getenv("AGENT_GM_DATA_KEY")
	if raw == "" {
		return c, errors.New("AGENT_GM_DATA_KEY is required: 256 bits as 64 hex characters or " +
			"standard base64. Generate one with `devbox run gen-secret`")
	}
	key, err := store.ParseDataKey(raw)
	if err != nil {
		return c, err
	}
	c.DataKey = key

	c.LogLevel = env("AGENT_GM_LOG_LEVEL", "info")
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return c, fmt.Errorf("AGENT_GM_LOG_LEVEL must be debug, info, warn or error, not %q", c.LogLevel)
	}
	c.LogFormat = env("AGENT_GM_LOG_FORMAT", "json")
	switch c.LogFormat {
	case "json", "text":
	default:
		return c, fmt.Errorf("AGENT_GM_LOG_FORMAT must be json or text, not %q", c.LogFormat)
	}

	// libgm at trace level base64-logs decrypted payloads, so trace is
	// refused unless AGENT_GM_UNSAFE_TRACE=1 is set (spec section 12.2).
	c.UnsafeTrace = os.Getenv("AGENT_GM_UNSAFE_TRACE") == "1"

	c.Chrome = os.Getenv("AGENT_GM_CHROME")
	c.DefaultAccount = os.Getenv("AGENT_GM_ACCOUNT")

	c.PairingTimeout = 5 * time.Minute
	if v := os.Getenv("AGENT_GM_PAIRING_TIMEOUT"); v != "" {
		d, err := ParseDuration(v)
		if err != nil {
			return c, fmt.Errorf("AGENT_GM_PAIRING_TIMEOUT: %w", err)
		}
		c.PairingTimeout = d
	}

	c.StateDir = StateDir()
	return c, nil
}

// LoadServer is Load plus the variables only the server needs:
// AGENT_GM_PUBLIC_URL, AGENT_GM_LISTEN_ADDR, AGENT_GM_ADMIN_SECRET and
// AGENT_GM_TRUSTED_PROXY_CIDRS.
//
// It is separate from Load because `agm` is the same binary and does not
// hold the admin secret, does not bind a port, and has no public URL of its
// own -- requiring them of every invocation would make the CLI unusable on a
// machine that is not also the server (spec sections 11.5, 15.1).
func LoadServer() (Config, error) {
	c, err := Load()
	if err != nil {
		return c, err
	}
	c.ListenAddr = env("AGENT_GM_LISTEN_ADDR", "0.0.0.0:8080")
	if err := c.loadPublicURL(); err != nil {
		return c, err
	}
	if err := c.loadAdminSecret(); err != nil {
		return c, err
	}
	if err := c.loadTrustedProxies(); err != nil {
		return c, err
	}
	return c, nil
}

// MinAdminSecretLength is spec section 12.1's floor. 43 characters is the
// length of a base64url-encoded 256-bit value, so a secret generated the
// documented way (`devbox run gen-secret`) always clears it and a
// hand-typed passphrase generally does not.
const MinAdminSecretLength = 43

func (c *Config) loadPublicURL() error {
	raw := os.Getenv("AGENT_GM_PUBLIC_URL")
	if raw == "" {
		return errors.New("AGENT_GM_PUBLIC_URL is required: it is the issuer, the canonical " +
			"resource and the base of every URL Agent GM hands out, and it is never derived " +
			"from a request's Host header")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("AGENT_GM_PUBLIC_URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("AGENT_GM_PUBLIC_URL must be an http or https URL, not %q", raw)
	}
	if u.Host == "" {
		return fmt.Errorf("AGENT_GM_PUBLIC_URL names no host: %q", raw)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("AGENT_GM_PUBLIC_URL carries a query or fragment: %q", raw)
	}
	// A trailing slash would produce doubled slashes in every URL built from
	// it, which breaks an audience comparison that is byte-exact by design.
	c.PublicURL = strings.TrimRight(u.String(), "/")
	return nil
}

func (c *Config) loadAdminSecret() error {
	s := os.Getenv("AGENT_GM_ADMIN_SECRET")
	if s == "" {
		return errors.New("AGENT_GM_ADMIN_SECRET is required: it is the owner bootstrap " +
			"credential. Generate one with `devbox run gen-secret`")
	}
	if len(s) < MinAdminSecretLength {
		// The value itself is never named in the error: it is the strongest
		// credential Agent GM issues (spec sections 12.1, 12.2).
		return fmt.Errorf("AGENT_GM_ADMIN_SECRET is %d characters; it must be at least %d. "+
			"Generate one with `devbox run gen-secret`", len(s), MinAdminSecretLength)
	}
	c.AdminSecret = s
	return nil
}

// loadTrustedProxies parses AGENT_GM_TRUSTED_PROXY_CIDRS.
//
// Unset or empty is the default and means forwarded headers are ignored
// ENTIRELY -- not "trusted from localhost", not "trusted if they look
// plausible". An invalid value refuses to start (spec section 12.3).
func (c *Config) loadTrustedProxies() error {
	raw := strings.TrimSpace(os.Getenv("AGENT_GM_TRUSTED_PROXY_CIDRS"))
	c.TrustedProxyRaw = raw
	if raw == "" {
		return nil
	}
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if p, err := netip.ParsePrefix(part); err == nil {
			c.TrustedProxyCIDRs = append(c.TrustedProxyCIDRs, p)
			continue
		}
		// A bare address is read as a host route, so an operator may write
		// either form.
		addr, err := netip.ParseAddr(part)
		if err != nil {
			return fmt.Errorf("AGENT_GM_TRUSTED_PROXY_CIDRS: %q is neither a CIDR nor an "+
				"address. Refusing to start rather than silently trusting nothing: an "+
				"operator who mistypes this should find out now", part)
		}
		c.TrustedProxyCIDRs = append(c.TrustedProxyCIDRs, netip.PrefixFrom(addr, addr.BitLen()))
	}
	if len(c.TrustedProxyCIDRs) == 0 {
		return fmt.Errorf("AGENT_GM_TRUSTED_PROXY_CIDRS is %q, which names no CIDR", raw)
	}
	return nil
}

// ClientSourceMode is what the one startup log line reports (spec section
// 12.3), so a misconfiguration where every caller collapses to one source is
// visible from the first line of the log.
func (c Config) ClientSourceMode() string {
	if len(c.TrustedProxyCIDRs) == 0 {
		return "socket_peer"
	}
	return "trusted_proxy"
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// StateDir is $XDG_STATE_HOME/agent-gm, where the kept Chrome profiles and
// the credentials file live (spec sections 11.4, 11.5).
func StateDir() string {
	if v := os.Getenv("XDG_STATE_HOME"); v != "" {
		return filepath.Join(v, "agent-gm")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "agent-gm")
	}
	return filepath.Join(home, ".local", "state", "agent-gm")
}

// ParseDuration accepts an integer plus a unit of s, m, h or d
// (spec section 11.1). Anything else is a usage error naming the value.
func ParseDuration(s string) (time.Duration, error) {
	if len(s) < 2 {
		return 0, fmt.Errorf("%q is not a duration: use an integer and one of s, m, h, d, like 30s or 2h", s)
	}
	unit := s[len(s)-1]
	n, err := strconv.Atoi(s[:len(s)-1])
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%q is not a duration: use an integer and one of s, m, h, d, like 30s or 2h", s)
	}
	switch unit {
	case 's':
		return time.Duration(n) * time.Second, nil
	case 'm':
		return time.Duration(n) * time.Minute, nil
	case 'h':
		return time.Duration(n) * time.Hour, nil
	case 'd':
		return time.Duration(n) * 24 * time.Hour, nil
	default:
		return 0, fmt.Errorf("%q is not a duration: the unit must be s, m, h or d", s)
	}
}
