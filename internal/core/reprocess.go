package core

import (
	"context"
	"fmt"
	"time"

	"github.com/thisnick/agent-gm/internal/store"
)

// PendingReprocessKey is the `server_meta` key of spec section 4.3.
//
// "A migration needing derived data recomputed sets
// `server_meta.pending_reprocess = <task name>`; the process runs that task
// once after startup and clears the key."
const PendingReprocessKey = "pending_reprocess"

// TaskReconcileParticipants is the task migration 0004 asks for.
//
// 0004 rewrites `messages.sender_participant` and `reactions.participant_id`
// to derived `part_` IDs by joining through `participants`, which already
// holds the mapping. What it cannot fix is a row whose participant Agent GM
// never ingested: there is nothing to join against. Those rows keep their raw
// value, and a raw value there makes every `sender` filter return an empty
// page -- silently, because an empty page is a valid answer.
//
// So the migration hands them here, and this re-ingests them through the
// current path, which derives correctly.
const TaskReconcileParticipants = "reconcile_participants"

// Sweeper is the reconciliation sweep, as the reprocess task needs it. It is
// declared here rather than imported so `internal/core` keeps knowing nothing
// about the supervisor.
type Sweeper interface {
	Sweep(ctx context.Context, accountID string, since time.Time) error
}

// RunPendingReprocess runs the task `server_meta.pending_reprocess` names, if
// any, and clears the key.
//
// **It is the missing half of section 4.3.** Migration 0004 was the first
// thing ever to write that key, and nothing read it: the clause says the
// process runs the task and clears the key, and without the second half the
// key stayed set for ever on every upgraded database while the rows it was
// set for kept their raw IDs. A migration that hands work to a task nothing
// runs has not deferred the work; it has dropped it.
//
// **It runs AFTER the listener binds**, on the goroutine that resumes the
// accounts. That is a deliberate reversal: it used to run first, on the
// reasoning that a caller must not see a half-reconciled database and read a
// short `sender=me` page as an answer. On the owner's real deployment the
// resume and the reprocess together took four minutes, and for those four
// minutes `/healthz` did not answer at all -- so every watcher concluded the
// service was down, which is a worse and much more frequent lie than a brief
// partial page.
//
// So the window is accepted and named: on the ONE start after an upgrade
// whose migration deferred work, a query filtered on a derived value -- the
// `sender=me` page is the one that matters -- can return fewer rows than it
// will a moment later, until this finishes and clears the key.
// `GET /v1/health` reports the task while it runs, so the state is visible
// rather than merely brief. What does NOT wait is anything that settles the
// database itself: migrations (section 4.3) and crash recovery (section 6.6)
// both complete before the socket is bound.
//
// **The key is cleared only when every account ROW was reconciled**, not
// merely every account that resumed. `accountIDs` is the set with a live
// client; every account in the store is relinked regardless, because a
// relink is a join and needs no client, and `signed_out` is an ordinary
// resting state whose history stays readable (section 4.7). Passing only the
// resumed set once meant "resumed accounts=0" followed by "cleared the key",
// with nothing rebuilt and no second attempt ever.
//
// A sweep that FAILS -- a phone asleep, an account that would not connect --
// leaves the key set, so the next start tries again rather than declaring the
// work done because it was attempted.
func RunPendingReprocess(ctx context.Context, st *store.Store, sweep Sweeper, accountIDs []string) (task string, ran bool, err error) {
	task, ok, err := st.Meta(ctx, PendingReprocessKey)
	if err != nil || !ok || task == "" {
		return "", false, err
	}

	// Every account ROW, not merely the accounts that resumed.
	//
	// This is the distinction R-17 turned on. `sup.List()` holds the
	// accounts with a live client; an account whose session is not on disk
	// does not resume, and `signed_out` is an ordinary resting state whose
	// history stays readable (section 4.7) -- which is exactly the history
	// whose contact links migration 0002 had to drop. Reconciling only the
	// resumed set meant "resumed accounts=0" followed by "cleared the key",
	// with nothing rebuilt and no second attempt ever.
	rows, err := st.Accounts(ctx)
	if err != nil {
		return task, false, fmt.Errorf("listing accounts for %s: %w", task, err)
	}
	offered := map[string]bool{}
	for _, id := range accountIDs {
		offered[id] = true
	}

	switch task {
	case TaskReconcileParticipants:
		for _, row := range rows {
			// The relink is a JOIN, so it runs for every account whether or
			// not it holds a client. It is the whole of what a database can
			// do for an account that cannot fetch: migration 0002 dropped
			// links it could not resolve while `contacts` was still empty,
			// and by now that table is populated for any account that ever
			// backfilled.
			if err := st.RelinkAccountContacts(ctx, row.ID); err != nil {
				return task, false, fmt.Errorf("relinking %s's contacts: %w", row.ID, err)
			}
			if !offered[row.ID] {
				// No client, so no fetch. Nothing further is possible now,
				// and nothing further is needed: when this account is paired
				// again, ordinary ingest links its participants through the
				// same path (section 4.7's "re-pairing resumes the same
				// rows"). Leaving the key set for it would make every start
				// re-walk every OTHER account for ever on account of one the
				// owner deliberately signed out.
				continue
			}
			if sweep == nil {
				return task, false, fmt.Errorf("%s is pending but no sweeper is wired", task)
			}
			// Since the epoch: the rows that need re-ingesting are exactly
			// the ones the sweep would otherwise skip as old. The sweep
			// re-fetches contacts as well as messages, so a link that needs
			// a contact Agent GM never held comes back too.
			if err := sweep.Sweep(ctx, row.ID, time.Time{}); err != nil {
				return task, false, fmt.Errorf("reconciling %s: %w", row.ID, err)
			}
		}
	default:
		// A key naming a task this build does not know is left ALONE, not
		// cleared: a newer binary wrote it, this one cannot do the work, and
		// clearing it would lose the request silently. It is reported so an
		// operator sees it.
		return task, false, fmt.Errorf("server_meta.pending_reprocess names %q, "+
			"which this build does not know how to run", task)
	}

	if err := st.SetMeta(ctx, PendingReprocessKey, ""); err != nil {
		return task, false, fmt.Errorf("clearing %s: %w", PendingReprocessKey, err)
	}
	return task, true, nil
}
