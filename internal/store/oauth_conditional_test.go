package store

import (
	"context"
	"testing"

	"github.com/thisnick/agent-gm/internal/clock"
)

// The three conditional UPDATEs of the OAuth tables, each asserted twice.
//
// Every one of them is a rule held by a WHERE clause -- `AND consumed_at_ms IS
// NULL`, `AND status = 'pending'`, `AND status = 'approved'` -- and a WHERE
// clause is exactly the kind of thing that can be deleted without a
// sequential test noticing, because sequentially the row is always in the
// state the caller expects. The second call is what tells the two
// implementations apart.
//
// A concurrency test over the HTTP surface does NOT do this job: the
// read-only lookup in front of each of these already sees the winner's write,
// so the losers are refused before they reach the UPDATE at all. That is
// belt and braces in production and a false pass in a test, and a reviewer's
// plant on the code consume survived exactly that way.

func openTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(t.TempDir(), clock.NewFake())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// seedRequest writes the client and the authorization request a code needs.
func seedRequest(t *testing.T, st *Store, requestID string) string {
	t.Helper()
	ctx := context.Background()
	clientID := ClientID()
	if err := st.AuthzTx(ctx, func(tx *AuthzTx) error {
		if err := tx.CreateOAuthClient(OAuthClient{
			ID: clientID, RedirectURIs: []string{"http://127.0.0.1/cb"},
			GrantTypes: []string{"authorization_code"}, ResponseTypes: []string{"code"},
			TokenEndpointAuthMethod: "none", MetadataJSON: "{}",
		}); err != nil {
			return err
		}
		return tx.CreateAuthorizationRequest(AuthorizationRequest{
			ID: requestID, ClientID: clientID, RedirectURI: "http://127.0.0.1/cb",
			State: "s", CodeChallenge: "c", CodeChallengeMethod: "S256",
			Resource: "https://gm.example.test/mcp", RequestedScopes: "messages:read",
			SelectedScopes: "messages:read", Status: AuthRequestPending,
			HandleHash: "h", FormTokenHash: "f", ExpiresAtMS: 1 << 40,
		})
	}); err != nil {
		t.Fatal(err)
	}
	return clientID
}

// TestAnAuthorizationCodeIsConsumedOnce is section 9.6's single-use rule at
// the level that enforces it.
//
// Plant P22: drop `AND consumed_at_ms IS NULL` from ConsumeAuthorizationCode.
// Planted 2026-09-07 by the reviewer; killed here.
func TestAnAuthorizationCodeIsConsumedOnce(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	requestID := AuthorizationRequestID()
	clientID := seedRequest(t, st, requestID)

	const hash = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	if err := st.AuthzTx(ctx, func(tx *AuthzTx) error {
		return tx.CreateAuthorizationCode(AuthorizationCode{
			CodeHash: hash, RequestID: requestID, ClientID: clientID,
			RedirectURI: "http://127.0.0.1/cb", CodeChallenge: "c",
			Scopes: "messages:read", Resource: "https://gm.example.test/mcp",
			ExpiresAtMS: 1 << 40,
		})
	}); err != nil {
		t.Fatal(err)
	}

	consume := func(authorizationID string) bool {
		var ok bool
		if err := st.AuthzTx(ctx, func(tx *AuthzTx) error {
			var cerr error
			ok, cerr = tx.ConsumeAuthorizationCode(hash, authorizationID)
			return cerr
		}); err != nil {
			t.Fatal(err)
		}
		return ok
	}

	if !consume("auth_first") {
		t.Fatal("the first consumption of a fresh code failed")
	}
	if consume("auth_second") {
		t.Fatal("a code was consumed TWICE. It is single-use, and the conditional UPDATE " +
			"is the only thing that makes that true when the read-only lookup in front " +
			"of it has not yet seen the winner's write")
	}

	// The first exchange's authorization is still the one recorded, so a
	// replay can revoke exactly what the leak bought.
	code, err := st.AuthorizationCodeByHash(ctx, hash)
	if err != nil {
		t.Fatal(err)
	}
	if code.AuthorizationID != "auth_first" {
		t.Errorf("the code records authorization %q; the second attempt overwrote the "+
			"first exchange's, so a replay would revoke the wrong grant -- or nothing",
			code.AuthorizationID)
	}
}

// TestAnAuthorizationRequestIsDecidedOnce and TestACompletedRequestMintsOneCode
// are the same rule on the two status transitions.
//
// Section 9.5: a no-longer-pending request is `idempotency_conflict`, and **a
// completed request can never mint a second code**.
func TestAnAuthorizationRequestIsDecidedOnce(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	requestID := AuthorizationRequestID()
	seedRequest(t, st, requestID)

	decide := func(status string) bool {
		var ok bool
		if err := st.AuthzTx(ctx, func(tx *AuthzTx) error {
			var derr error
			ok, derr = tx.DecideAuthorizationRequest(requestID, status, "messages:read", "")
			return derr
		}); err != nil {
			t.Fatal(err)
		}
		return ok
	}

	if !decide(AuthRequestApproved) {
		t.Fatal("the first decision on a pending request failed")
	}
	if decide(AuthRequestDenied) {
		t.Fatal("a decided request was decided again; an owner's approval must not be " +
			"overwritable by a second call that arrives after it")
	}
}

func TestACompletedRequestMintsOneCode(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	requestID := AuthorizationRequestID()
	seedRequest(t, st, requestID)

	complete := func() bool {
		var ok bool
		if err := st.AuthzTx(ctx, func(tx *AuthzTx) error {
			var cerr error
			ok, cerr = tx.CompleteAuthorizationRequest(requestID)
			return cerr
		}); err != nil {
			t.Fatal(err)
		}
		return ok
	}

	// A PENDING request cannot be completed at all: there is no
	// self-service, and the owner has to approve first.
	if complete() {
		t.Fatal("a pending request was completed without an approval")
	}

	if err := st.AuthzTx(ctx, func(tx *AuthzTx) error {
		_, derr := tx.DecideAuthorizationRequest(requestID, AuthRequestApproved, "messages:read", "")
		return derr
	}); err != nil {
		t.Fatal(err)
	}

	if !complete() {
		t.Fatal("an approved request could not be completed")
	}
	if complete() {
		t.Fatal("a completed request was completed a second time; section 9.5 says it can " +
			"never mint a second code")
	}
}

// TestAnEnrollmentCodeIsRedeemedOnce is the fourth of the same shape: one
// code, one authorization request.
func TestAnEnrollmentCodeIsRedeemedOnce(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	id := EnrollmentCodeID()
	if err := st.AuthzTx(ctx, func(tx *AuthzTx) error {
		return tx.CreateEnrollmentCode(EnrollmentCode{
			ID: id, CodeHash: "cafe", Label: "test",
			Scopes: "messages:read", ExpiresAtMS: 1 << 40,
		})
	}); err != nil {
		t.Fatal(err)
	}

	consume := func(requestID string) bool {
		var ok bool
		if err := st.AuthzTx(ctx, func(tx *AuthzTx) error {
			var cerr error
			ok, cerr = tx.ConsumeEnrollmentCode(id, requestID)
			return cerr
		}); err != nil {
			t.Fatal(err)
		}
		return ok
	}

	if !consume("authreq_first") {
		t.Fatal("the first redemption of a fresh enrollment code failed")
	}
	if consume("authreq_second") {
		t.Fatal("one enrollment code raised TWO authorization requests; the owner issued " +
			"one grant, not two")
	}
}
