package main

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/thisnick/agent-gm/internal/clock"
	"github.com/thisnick/agent-gm/internal/core"
	"github.com/thisnick/agent-gm/internal/gm"
	"github.com/thisnick/agent-gm/internal/store"
)

// R-17. The reprocess task was handed `sup.List()` -- the accounts with a
// live client -- and cleared the key regardless of what that list contained.
// On a database whose only account had no session, the log read
// "resumed accounts=0" and then "ran the pending reprocess task and cleared
// the key", with nothing rebuilt and no second attempt ever.
//
// That is the exact failure the task exists to prevent, one level up: a
// migration that hands work to a task nothing runs has dropped it, and so has
// a task that reports success for work it never reached. And the history it
// abandons is a `signed_out` account's, which section 4.7 promises stays
// readable.
//
// Plant: pass sup.List() to the relink loop instead of st.Accounts and this
// fails at "the signed-out account's contact link was not rebuilt". Planted
// 2026-09-07.
func TestReprocessReachesAnAccountThatDidNotResume(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(dir, clock.NewFake())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	// An account with history and NO session on disk: signed out, which
	// section 4.7 makes an ordinary resting state.
	const address = "signed-out@example.test"
	id := store.AccountID(address)
	if err := st.UpsertAccount(ctx, store.Account{
		ID: id, GoogleAccount: address, State: store.StateSignedOut, SessionPresent: false,
	}); err != nil {
		t.Fatal(err)
	}
	convID, err := st.UpsertConversation(ctx, id, gm.Conversation{
		SourceID: "conv-so", Name: "Signed out", Type: gm.ConversationTypeRCS,
		SendModeRaw: gm.SendModeAuto, Folder: gm.FolderInbox,
		Participants: []gm.Participant{
			{SourceID: "them", PhoneE164: "+12025550123", DisplayName: "Fixture", IsVisible: true},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	// A contact this account holds, which migration 0002 could not link
	// because `contacts` was empty when it ran.
	if _, err := st.UpsertContact(ctx, id,
		gm.Contact{SourceID: "them", DisplayName: "Fixture", PhoneE164: "+12025550123"},
		""); err != nil {
		t.Fatal(err)
	}
	if err := st.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE participants SET contact_id = NULL WHERE conversation_id = ?`, convID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetMeta(ctx, core.PendingReprocessKey, core.TaskReconcileParticipants); err != nil {
		t.Fatal(err)
	}

	// No account resumed, which is what serve passes when no session is on
	// disk. The task must still reach this account's rows.
	task, ran, err := core.RunPendingReprocess(ctx, st, refusingSweeper{t}, nil)
	if err != nil {
		t.Fatalf("RunPendingReprocess: %v", err)
	}
	if !ran || task != core.TaskReconcileParticipants {
		t.Fatalf("task=%q ran=%v", task, ran)
	}

	var linked int
	if err := st.Reader().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM participants WHERE conversation_id = ? AND contact_id IS NOT NULL`,
		convID).Scan(&linked); err != nil {
		t.Fatal(err)
	}
	if linked == 0 {
		t.Error("the signed-out account's contact link was not rebuilt; its history is " +
			"readable (4.7) and its links are now permanently wrong")
	}
}

// refusingSweeper fails if it is called: a signed-out account has no client,
// so nothing may try to sweep it.
type refusingSweeper struct{ t *testing.T }

func (r refusingSweeper) Sweep(context.Context, string, time.Time) error {
	r.t.Error("the task tried to sweep an account with no client")
	return nil
}
