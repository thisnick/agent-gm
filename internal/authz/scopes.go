// Package authz is Agent GM's credential logic: what a scope is, how the
// admin bootstrap session of spec section 9.7 is minted and refreshed, how a
// presented token is authenticated, and every limiter that stands in front of
// those three (spec sections 9.6, 9.8, 12.1, 12.3).
//
// Slice 2 mints exactly one kind of authorization -- the admin bootstrap --
// but the shape is the OAuth one, because Slice 3 adds a second token SOURCE
// and not a second security model.
//
// Two rules run through the whole package and are worth stating once:
//
//   - Every check of a caller-supplied value against a stored one is
//     constant-time over fixed-length hashes, never `==` on strings and never
//     bytes.Equal (spec section 12.1). See compare.go, which is the only file
//     permitted to compare a secret-derived value, and
//     comparison_audit_test.go, which enforces that by parsing this package's
//     own source.
//   - Every timeout is measured on the injected clock (spec section 13.1). No
//     function here calls time.Now, time.Sleep or time.After, and no test
//     waits anything out.
package authz

import (
	"fmt"
	"sort"
	"strings"
)

// Scope is one entry of spec section 9.7's table.
type Scope string

const (
	// ScopeMessagesRead grants every read route and read tool, whoami, logout
	// and download-ticket redemption.
	ScopeMessagesRead Scope = "messages:read"
	// ScopeMessagesWrite grants send, start, mark read, reactions, uploads,
	// get_operation and upload-ticket redemption.
	ScopeMessagesWrite Scope = "messages:write"
	// ScopeMessagesDelete grants delete_message and delete_conversation, and
	// nothing else.
	ScopeMessagesDelete Scope = "messages:delete"
	// ScopeAdmin grants /v1/admin/* and all pairing routes. It is issued only
	// by the admin bootstrap, is never enrollable, and is invalid_scope at
	// /oauth/authorize. The only way to hold it is the admin secret.
	ScopeAdmin Scope = "admin"
)

// canonicalOrder is the order scopes are rendered in, everywhere: the order
// spec section 9.7 writes the admin bootstrap grant in. Rendering is
// deterministic so that two equal scope sets have one string form, which is
// what makes an audit payload and a stored column comparable.
var canonicalOrder = []Scope{ScopeAdmin, ScopeMessagesRead, ScopeMessagesWrite, ScopeMessagesDelete}

// AdminBootstrapScopes is what POST /v1/auth/admin-session mints when no
// narrowing is asked for: `admin` plus all three messaging scopes.
//
// The owner presenting AGENT_GM_ADMIN_SECRET is by definition the person the
// whole service belongs to, and a credential that could administer the server
// but not read a message would be useless (spec section 9.7).
func AdminBootstrapScopes() ScopeSet { return NewScopeSet(canonicalOrder...) }

// Enrollable reports whether a scope may ever be granted by enrollment or at
// /oauth/authorize. `admin` never is.
func (s Scope) Enrollable() bool { return s != ScopeAdmin }

// Known reports whether the scope is one of the four.
func (s Scope) Known() bool {
	for _, c := range canonicalOrder {
		if c == s {
			return true
		}
	}
	return false
}

// ScopeSet is an unordered set of scopes with a canonical rendering.
type ScopeSet struct {
	m map[Scope]struct{}
}

// NewScopeSet builds a set. Duplicates collapse.
func NewScopeSet(scopes ...Scope) ScopeSet {
	s := ScopeSet{m: make(map[Scope]struct{}, len(scopes))}
	for _, sc := range scopes {
		s.m[sc] = struct{}{}
	}
	return s
}

// ParseScopes builds a set from caller-supplied strings, refusing any scope
// outside the four of spec section 9.7. An unknown scope is invalid_scope
// rather than silently dropped: a caller who asks for `messages:reed` should
// find out, not receive a session quietly missing a permission.
func ParseScopes(raw []string) (ScopeSet, error) {
	s := ScopeSet{m: make(map[Scope]struct{}, len(raw))}
	for _, r := range raw {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		sc := Scope(r)
		if !sc.Known() {
			return ScopeSet{}, fmt.Errorf("%w: unknown scope %q", ErrInvalidScope, r)
		}
		s.m[sc] = struct{}{}
	}
	return s, nil
}

// ParseScopeString parses a stored space-joined column.
func ParseScopeString(s string) (ScopeSet, error) { return ParseScopes(strings.Fields(s)) }

// Empty reports whether the set holds no scopes.
func (s ScopeSet) Empty() bool { return len(s.m) == 0 }

// Len is the number of scopes held.
func (s ScopeSet) Len() int { return len(s.m) }

// Has reports membership.
func (s ScopeSet) Has(sc Scope) bool {
	_, ok := s.m[sc]
	return ok
}

// List renders the set in canonical order. Any scope outside the four (which
// ParseScopes refuses, so this can only come from NewScopeSet) sorts after
// them, alphabetically, so the rendering is still total.
func (s ScopeSet) List() []Scope {
	out := make([]Scope, 0, len(s.m))
	for _, c := range canonicalOrder {
		if s.Has(c) {
			out = append(out, c)
		}
	}
	var rest []Scope
	for sc := range s.m {
		if !sc.Known() {
			rest = append(rest, sc)
		}
	}
	sort.Slice(rest, func(i, j int) bool { return rest[i] < rest[j] })
	return append(out, rest...)
}

// Strings renders the set as a []string in canonical order.
func (s ScopeSet) Strings() []string {
	l := s.List()
	out := make([]string, len(l))
	for i, sc := range l {
		out[i] = string(sc)
	}
	return out
}

// String renders the set space-joined in canonical order, which is exactly how
// authorizations.scopes and authorizations.minted_scopes store it.
func (s ScopeSet) String() string { return strings.Join(s.Strings(), " ") }

// Subset reports whether every scope of s is also in other -- that is, whether
// s is a narrowing of other (or equal to it).
func (s ScopeSet) Subset(other ScopeSet) bool {
	for sc := range s.m {
		if !other.Has(sc) {
			return false
		}
	}
	return true
}

// Equal reports set equality.
func (s ScopeSet) Equal(other ScopeSet) bool {
	return len(s.m) == len(other.m) && s.Subset(other)
}

// Widens reports whether s asks for anything other does not already hold. It
// is the negation of Subset, named for the thing spec section 9.6 forbids so
// that call sites read as the rule they enforce.
func (s ScopeSet) Widens(other ScopeSet) bool { return !s.Subset(other) }

// Clone returns an independent copy.
func (s ScopeSet) Clone() ScopeSet {
	out := ScopeSet{m: make(map[Scope]struct{}, len(s.m))}
	for sc := range s.m {
		out.m[sc] = struct{}{}
	}
	return out
}
