package authz

import (
	"context"
	"errors"
	"testing"

	"github.com/thisnick/agent-gm/internal/clock"
	"github.com/thisnick/agent-gm/internal/store"
)

// TestARefreshThatLosesTheRaceIsReuse stands in the window between the
// read-only lookup of a presented refresh token and the transaction that
// rotates it.
//
// The rule is spec section 9.6's: a rotation, its audit record, and the family
// revocation that reuse triggers all commit in ONE transaction -- which is
// only meaningful because that transaction RE-READS the row it is about to
// spend. The read-only lookup before it cannot be the check: two callers both
// pass it, and by construction neither can see what the other is about to do.
//
// No concurrent test lands inside a window this narrow reliably -- a
// concurrency test over the HTTP surface passes with the guard REMOVED,
// because the read-only lookup already sees the winner's write -- and a
// reviewer's plant removing the re-read survived the whole suite. The fault
// seam is how a test stands in that window on purpose, which is the same
// argument HandlerDeps.AfterErase makes for the erasure crash window.
//
// It is an internal test because spending a token by hand needs tokenHash,
// and exporting a way to spend a token would be a bigger hole than the test
// is worth.
//
// Plant P4: delete the `if cur.Spent() || cur.Revoked()` re-read in
// refreshSession's transaction. Planted 2026-09-07 by the reviewer; killed
// here.
func TestARefreshThatLosesTheRaceIsReuse(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake()
	const secret = "a-test-admin-secret-well-over-the-43-character-minimum-0123456789"

	st, err := store.Open(t.TempDir(), clk)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	svc, err := New(st, clk, NewMemorySettings(), nil, Config{
		AdminSecret: secret,
		PublicURL:   "https://gm.agent-wx.app",
	})
	if err != nil {
		t.Fatal(err)
	}

	session, err := svc.MintAdminSession(ctx, secret, nil, "203.0.113.9")
	if err != nil {
		t.Fatal(err)
	}

	// The other caller wins the race: it spends the very token this refresh
	// is about to rotate, after the read-only lookup has already seen it
	// unspent.
	spent := false
	svc.BeforeRefreshTx = func() {
		if spent {
			return
		}
		spent = true
		hash := tokenHash(svc.audience, store.TokenKindRefresh, session.RefreshToken)
		if terr := st.AuthzTx(ctx, func(tx *store.AuthzTx) error {
			return tx.SpendToken(hash)
		}); terr != nil {
			t.Errorf("standing in the window: %v", terr)
		}
	}

	_, err = svc.Refresh(ctx, session.RefreshToken, nil, "203.0.113.9")
	if err == nil {
		t.Fatal("a refresh that LOST the race rotated anyway. The transaction has to " +
			"re-read the row before spending it: the read-only lookup cannot see what " +
			"another caller is about to do")
	}
	if !errors.Is(err, ErrTokenReused) {
		t.Errorf("the refusal is %v, want ErrTokenReused", err)
	}

	// Reuse revokes the family and the authorization, so the access token
	// the mint produced is dead too.
	if _, aerr := svc.Authenticate(ctx, session.AccessToken); aerr == nil {
		t.Error("after losing the race the session's access token still authenticates; " +
			"reuse revokes the whole family and its authorization")
	}
}
