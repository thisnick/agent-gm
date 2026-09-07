package cli

// The admin bootstrap scope set is written out in this package, because `agm`
// is a REST client and imports no server package: `internal/authz` is not in
// the shipped binary's import graph and is not going to be.
//
// That is a decision about the PRODUCTION build, and a test binary is not the
// production build -- `_test.go` files are excluded from the package's ordinary
// compilation, so importing `internal/authz` here costs `agm` nothing and buys
// the one thing the copy was missing: something that fails when the two drift.
//
// The drift that matters is not the one that is easy to imagine. Adding a
// scope to the CLI's list is caught by
// `TestAdminLoginSuggestsNarrowingWhenItGrantsAllFourScopes`, which stops
// seeing the hint. The reverse is silent: the server gains a fifth scope,
// `canonicalOrder` grows, this list stays at four, `grantsEveryAdminScope`
// answers false for every un-narrowed session, and the narrowing hint simply
// stops appearing -- with nothing red, because the stub the CLI test asserts
// against is a third hand-written copy of the same four strings and would not
// have grown either. That stub is now derived from the same source, and this is
// the test that ties the two lists together.

import (
	"testing"

	"github.com/thisnick/agent-gm/internal/authz"
)

func TestTheCLIsAdminScopeListIsTheServersAdminScopeList(t *testing.T) {
	want := authz.AdminBootstrapScopes().Strings()

	if len(adminBootstrapScopes) != len(want) {
		t.Fatalf("the CLI lists %d admin bootstrap scopes and the server mints %d:\n"+
			"  cli:    %v\n  server: %v\n"+
			"grantsEveryAdminScope compares the granted set against the CLI's list, so a "+
			"disagreement means the narrowing hint is printed for a narrowed session or "+
			"never printed at all",
			len(adminBootstrapScopes), len(want), adminBootstrapScopes, want)
	}

	have := make(map[string]bool, len(adminBootstrapScopes))
	for _, s := range adminBootstrapScopes {
		have[s] = true
	}
	for _, w := range want {
		if !have[w] {
			t.Errorf("the server mints %q in an un-narrowed admin session and the CLI does not "+
				"list it:\n  cli:    %v\n  server: %v", w, adminBootstrapScopes, want)
		}
	}
}

// And the helper itself, on the two answers that matter: an un-narrowed
// session, and one narrowed to what a read-only machine asks for.
func TestGrantsEveryAdminScope(t *testing.T) {
	if !grantsEveryAdminScope(authz.AdminBootstrapScopes().Strings()) {
		t.Error("an un-narrowed session was not recognised as un-narrowed, so it gets no hint")
	}
	// Order is not part of the question: the set is.
	if !grantsEveryAdminScope([]string{"messages:delete", "messages:write", "messages:read", "admin"}) {
		t.Error("the same set in another order was not recognised; this compares as a set")
	}
	for _, narrowed := range [][]string{
		{"admin", "messages:read"},
		{"admin"},
		{"messages:read", "messages:write", "messages:delete"},
		// Four scopes, one of them not a scope the server mints: a count would
		// have called this un-narrowed.
		{"admin", "messages:read", "messages:write", "messages:archive"},
		nil,
	} {
		if grantsEveryAdminScope(narrowed) {
			t.Errorf("%v was treated as an un-narrowed session; it would be told to narrow "+
				"a session that is already narrow", narrowed)
		}
	}
}
