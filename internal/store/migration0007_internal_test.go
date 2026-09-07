package store

import (
	"context"
	"database/sql"
	"testing"

	"github.com/thisnick/agent-gm/internal/clock"
)

// Migration 0007 (D38) makes `operations.idempotency_key` nullable and turns
// the inline UNIQUE into a partial index.
//
// The half worth a test is not "the column is nullable" -- that is one
// pragma -- but the half that would be a silent data-loss bug if the
// migration had used `''` instead of NULL: **two keyless operations must be
// able to coexist for one authorization, one account and one kind.** With an
// empty string in the column the second INSERT would violate the unique
// constraint, the send would be refused as a duplicate, and a real message
// would go missing while everything above reported an ordinary error.
//
// Plant: change the partial index to an unconditional one, or write `''`
// instead of NULL in InsertOperation, and this fails on the second insert.
// Planted 2026-09-07.
func TestMigration0007LetsKeylessOperationsCoexist(t *testing.T) {
	ctx := context.Background()
	st, _ := newInternalStore(t)

	seedAccountForOperations(t, st, "acct-1")

	insert := func(id string) error {
		return st.InsertOperation(ctx, Operation{
			ID: id, AccountID: "acct-1", Kind: "send_text",
			AuthorizationID: "auth-1", RequestFingerprint: "fp-" + id,
			RequestPayloadJSON: `{}`,
		})
	}
	if err := insert("op-1"); err != nil {
		t.Fatalf("the first keyless operation was refused: %v", err)
	}
	if err := insert("op-2"); err != nil {
		t.Fatalf("a SECOND keyless operation was refused: %v\n"+
			"That is the empty-string bug migration 0007 exists to avoid: the second "+
			"send would be reported as a duplicate and a real message would go missing.", err)
	}

	// Both rows come back with an empty key, which is how the Go side spells
	// NULL, and neither has adopted the other's row.
	for _, id := range []string{"op-1", "op-2"} {
		op, err := st.Operation(ctx, id)
		if err != nil {
			t.Fatalf("reading %s: %v", id, err)
		}
		if op.IdempotencyKey != "" {
			t.Errorf("%s stored the key %q; a keyless operation stores NULL", id, op.IdempotencyKey)
		}
	}

	// And a KEYED pair still collides, so the partial index is a real index
	// rather than an index that never applies. Without this assertion the
	// test above would pass on a migration that dropped uniqueness entirely,
	// which would silently turn every replay into a second message.
	keyed := func(id string) error {
		return st.InsertOperation(ctx, Operation{
			ID: id, AccountID: "acct-1", Kind: "send_text",
			AuthorizationID: "auth-1", IdempotencyKey: "k1",
			RequestFingerprint: "fp", RequestPayloadJSON: `{}`,
		})
	}
	if err := keyed("op-3"); err != nil {
		t.Fatalf("the first keyed operation was refused: %v", err)
	}
	if err := keyed("op-4"); err == nil {
		t.Fatal("a second operation under the SAME key was accepted; " +
			"the uniqueness of section 6.3 is gone and every replay is now a second message")
	}

	// OperationByKey finds the keyed one and refuses to answer for the
	// keyless ones, which would otherwise all look alike.
	if got, err := st.OperationByKey(ctx, "auth-1", "acct-1", "send_text", "k1"); err != nil {
		t.Fatalf("looking up the keyed operation: %v", err)
	} else if got.ID != "op-3" {
		t.Errorf("OperationByKey returned %s, want op-3", got.ID)
	}
	if _, err := st.OperationByKey(ctx, "auth-1", "acct-1", "send_text", ""); err == nil {
		t.Fatal("OperationByKey answered for the empty key; a keyless operation is not " +
			"addressable by key, and matching one would make every keyless send a replay of the first")
	}

	// The mirror hazard's query must not fire on the empty key either: with
	// two accounts holding keyless operations it would otherwise report a
	// crossing that never happened and refuse every second account's send.
	if _, used, err := st.KeyUsedByAnotherAccount(ctx, "auth-1", "send_text", "", "acct-2"); err != nil {
		t.Fatalf("KeyUsedByAnotherAccount: %v", err)
	} else if used {
		t.Fatal("the empty key was reported as used by another account; every keyless send " +
			"from a second account would be refused")
	}
}

func seedAccountForOperations(t *testing.T, st *Store, id string) {
	t.Helper()
	ctx := context.Background()
	if err := st.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO accounts (id, google_account, state, created_at_ms, updated_at_ms)
			 VALUES (?, ?, 'connected', 1, 1)`, id, id+"@example.test")
		return err
	}); err != nil {
		t.Fatalf("seeding %s: %v", id, err)
	}
}

func newInternalStore(t *testing.T) (*Store, *clock.Fake) {
	t.Helper()
	c := clock.NewFake()
	st, err := Open(t.TempDir(), c)
	if err != nil {
		t.Fatalf("opening a store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st, c
}
