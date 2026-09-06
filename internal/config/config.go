// Package config reads Agent GM's environment. There is no config file
// (spec section 15.1), and secrets are read from nowhere but the environment.
package config

import (
	"errors"
	"fmt"
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
