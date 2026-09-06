package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/thisnick/agent-gm/internal/gm"
)

// ErrPasteUnreadable means the paste was neither a JSON object of cookie name
// to value nor a cURL command carrying a Cookie header.
var ErrPasteUnreadable = errors.New("could not read cookies from that paste")

// MissingCookiesError names the required cookies a paste did not carry, and
// the domain each comes from -- because a paste scoped to .google.com alone
// silently omits OSID, which is the most common way this fails.
type MissingCookiesError struct {
	Missing []string
}

func (e *MissingCookiesError) Error() string {
	var b strings.Builder
	b.WriteString("that paste is missing ")
	for i, name := range e.Missing {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%s (from %s)", name, gm.GaiaCookieDomains[name])
	}
	b.WriteString(".\n\nOSID is host-scoped to messages.google.com, so a copy taken " +
		"from a .google.com request does not carry it. Copy a request to " +
		"messages.google.com instead.")
	return b.String()
}

// ParsePaste accepts either a JSON object of cookie name to value or a cURL
// command copied from browser devtools, matching upstream's own instruction
// text (connector/login.go:227).
//
// The input is parsed once, never echoed and never copied to disk.
func ParsePaste(input string) (map[string]string, error) {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return nil, ErrPasteUnreadable
	}

	var cookies map[string]string
	if strings.HasPrefix(trimmed, "{") {
		var raw map[string]string
		if err := json.Unmarshal([]byte(trimmed), &raw); err != nil {
			return nil, fmt.Errorf("%w: it starts like JSON but does not parse", ErrPasteUnreadable)
		}
		cookies = raw
	} else {
		cookies = cookiesFromCurl(trimmed)
	}
	if len(cookies) == 0 {
		return nil, ErrPasteUnreadable
	}

	// Keep exactly the seven, and nothing else: a devtools copy carries far
	// more than Agent GM needs, and none of the rest is stored.
	kept := map[string]string{}
	for _, name := range append(append([]string{}, gm.GaiaRequiredCookies...), gm.GaiaOptionalCookies...) {
		if v, ok := cookies[name]; ok && v != "" {
			kept[name] = v
		}
	}
	if missing := gm.MissingRequiredCookies(kept); len(missing) > 0 {
		sort.Strings(missing)
		return nil, &MissingCookiesError{Missing: missing}
	}
	return kept, nil
}

// cookiesFromCurl pulls the Cookie header out of a cURL command. Chrome's
// "Copy as cURL" writes `-H 'cookie: a=b; c=d'`; other browsers write
// `--header` or `-b`.
func cookiesFromCurl(cmd string) map[string]string {
	tokens := shellSplit(cmd)
	out := map[string]string{}
	for i := 0; i < len(tokens); i++ {
		t := tokens[i]
		var value string
		switch {
		case t == "-H" || t == "--header" || t == "-b" || t == "--cookie":
			if i+1 >= len(tokens) {
				continue
			}
			i++
			value = tokens[i]
		case strings.HasPrefix(t, "-H"):
			value = strings.TrimPrefix(t, "-H")
		case strings.HasPrefix(t, "--header="):
			value = strings.TrimPrefix(t, "--header=")
		case strings.HasPrefix(t, "-b"):
			value = strings.TrimPrefix(t, "-b")
		default:
			continue
		}
		lower := strings.ToLower(value)
		if idx := strings.Index(lower, "cookie:"); idx >= 0 {
			value = value[idx+len("cookie:"):]
		}
		for _, pair := range strings.Split(value, ";") {
			pair = strings.TrimSpace(pair)
			eq := strings.Index(pair, "=")
			if eq <= 0 {
				continue
			}
			name := strings.TrimSpace(pair[:eq])
			val := strings.TrimSpace(pair[eq+1:])
			if name != "" && val != "" {
				out[name] = val
			}
		}
	}
	return out
}

// shellSplit is a small POSIX-ish tokeniser: enough for a devtools cURL
// paste, which uses single quotes on Unix and double quotes on Windows, plus
// backslash line continuations.
func shellSplit(s string) []string {
	var tokens []string
	var cur strings.Builder
	var quote rune
	started := false

	flush := func() {
		if started {
			tokens = append(tokens, cur.String())
			cur.Reset()
			started = false
		}
	}
	runes := []rune(s)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
				continue
			}
			if quote == '"' && r == '\\' && i+1 < len(runes) {
				i++
				cur.WriteRune(runes[i])
				started = true
				continue
			}
			cur.WriteRune(r)
			started = true
		case r == '\'' || r == '"':
			quote = r
			started = true
		case r == '\\' && i+1 < len(runes):
			i++
			if runes[i] == '\n' || runes[i] == '\r' {
				continue // a line continuation
			}
			cur.WriteRune(runes[i])
			started = true
		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
			flush()
		case r == '^' && i+1 < len(runes) && (runes[i+1] == '\n' || runes[i+1] == '\r'):
			i++ // a Windows cmd line continuation
		default:
			cur.WriteRune(r)
			started = true
		}
	}
	flush()
	return tokens
}
