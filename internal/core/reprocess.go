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
// It runs BEFORE the listener binds, for the same reason crash recovery does
// (section 6.6): a caller must not see a half-reconciled database and read an
// empty `sender=me` page as an answer.
//
// The key is cleared only when every account was reconciled. A sweep that
// failed -- a phone that is asleep, an account that would not connect --
// leaves the key set, so the next start tries again rather than declaring the
// work done because it was attempted.
func RunPendingReprocess(ctx context.Context, st *store.Store, sweep Sweeper, accountIDs []string) (task string, ran bool, err error) {
	task, ok, err := st.Meta(ctx, PendingReprocessKey)
	if err != nil || !ok || task == "" {
		return "", false, err
	}
	switch task {
	case TaskReconcileParticipants:
		if sweep == nil {
			return task, false, fmt.Errorf("%s is pending but no sweeper is wired", task)
		}
		for _, id := range accountIDs {
			// Since the epoch: the rows that need re-ingesting are exactly
			// the ones the sweep would otherwise skip as old. The sweep
			// re-fetches contacts as well as messages, so the links
			// migration 0002 had to drop come back.
			if err := sweep.Sweep(ctx, id, time.Time{}); err != nil {
				return task, false, fmt.Errorf("reconciling %s: %w", id, err)
			}
			// And relink explicitly, because a participant whose contact was
			// already in the database needs no fetch -- only the join that
			// migration 0002 could not do while `contacts` was still empty.
			if err := st.RelinkAccountContacts(ctx, id); err != nil {
				return task, false, fmt.Errorf("relinking %s's contacts: %w", id, err)
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
