package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/thisnick/agent-gm/internal/gm"
	"github.com/thisnick/agent-gm/internal/store"
)

// Reconcile is the reconciliation sweep of spec section 5.4, for ONE account.
//
// The live event stream is not a complete record. The library's dedup
// abandons every remaining part of a batch on a hit (section 3.4,
// event_handler.go:263-266,272-275), so messages can be LOST, not merely
// duplicated, and no amount of local dedup recovers what never arrived. This
// is the only thing that does.
//
//  1. ListConversations(active) and, if backfill.include_archive,
//     ListConversations(archived).
//  2. For each conversation whose last_activity_ms is newer than `since`, or
//     whose latest_message_id is not a row we hold:
//     FetchMessages(convID, page_size, cursor=nil), walking back until a
//     message older than `since` is reached.
//  3. Upsert everything through the same path as section 5.3. Unchanged rows
//     write nothing (content_hash).
//  4. Record accounts.last_sweep_at_ms for this account.
//
// It is idempotent by construction, because every write goes through the same
// upsert functions the live stream uses. Running it more often costs
// bandwidth and nothing else.
//
// `since` is supplied by the trigger, from the column section 5.4's table
// names -- last_event_at_ms for every event-driven trigger, last_sweep_at_ms
// for the timer, the epoch for a whole-server re-backfill. It is not the
// current time: a sweep that walked back only as far as "now" would re-list
// the conversations and then decline to fetch the very messages it exists to
// recover.
func Reconcile(ctx context.Context, a *Account, since time.Time) error {
	return a.Reconcile(ctx, since)
}

// Reconcile is the method form of the package-level Reconcile.
func (a *Account) Reconcile(ctx context.Context, since time.Time) error {
	sinceMS := since.UnixMilli()
	if since.IsZero() {
		sinceMS = 0
	}
	in := a.Ingest()

	// 1.
	convs, err := a.Backend.ListConversations(ctx, gm.FolderInbox, a.Config.ConversationPageSize)
	if err != nil {
		return fmt.Errorf("sweep: listing inbox conversations: %w", err)
	}
	if a.Config.IncludeArchive {
		arch, err := a.Backend.ListConversations(ctx, gm.FolderArchive, a.Config.ConversationPageSize)
		if err != nil {
			return fmt.Errorf("sweep: listing archived conversations: %w", err)
		}
		convs = append(convs, arch...)
	}

	for _, c := range convs {
		// 2. The decision is made against what is STORED, before the
		// conversation upsert below overwrites it: after the upsert, a
		// conversation whose newest message we lost looks identical to one we
		// are up to date with.
		need, err := a.needsFetch(ctx, c, sinceMS)
		if err != nil {
			return err
		}
		if _, err := in.IngestConversation(ctx, c); err != nil {
			return err
		}
		if !need {
			continue
		}
		if err := a.sweepConversation(ctx, c, sinceMS); err != nil {
			return err
		}
	}

	// 4. THIS account's row.
	if err := a.setLastSweep(ctx, a.now()); err != nil {
		return err
	}
	a.sweeps.Add(1)
	return nil
}

func (a *Account) needsFetch(ctx context.Context, c gm.Conversation, sinceMS int64) (bool, error) {
	stored, err := a.Store.Conversation(ctx, store.ConversationID(a.ID, c.SourceID))
	if errors.Is(err, sql.ErrNoRows) {
		// A conversation we have never seen: everything in it is missing.
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("sweep: reading conversation: %w", err)
	}
	activity := stored.LastActivityMS
	if !c.LastActivity.IsZero() && c.LastActivity.UnixMilli() > activity {
		activity = c.LastActivity.UnixMilli()
	}
	if activity > sinceMS {
		return true, nil
	}
	// The other half of the rule, and the half that catches real loss: the
	// thread's newest message, as Google reports it, is not a row we hold.
	if c.LatestMessageID != "" {
		want := store.MessageID(a.ID, c.SourceID, c.LatestMessageID)
		if _, err := a.Store.Message(ctx, want); errors.Is(err, sql.ErrNoRows) {
			return true, nil
		} else if err != nil {
			return false, fmt.Errorf("sweep: checking latest message: %w", err)
		}
	}
	return false, nil
}

// sweepConversation walks one thread back until a message older than `since`
// is reached. Pages are newest-first, so the first message below the bound
// ends the walk: everything beyond it is older still and was already ingested
// by whatever produced `since`.
func (a *Account) sweepConversation(ctx context.Context, c gm.Conversation, sinceMS int64) error {
	in := a.Ingest()
	isDM := !c.IsGroup
	var cursor *gm.Cursor
	for {
		page, next, err := a.Backend.ListMessages(ctx, c.SourceID, a.Config.MessagePageSize, cursor)
		if err != nil {
			return fmt.Errorf("sweep: fetching messages: %w", err)
		}
		if len(page) == 0 {
			return nil
		}
		for _, m := range page {
			if !m.Timestamp.IsZero() && m.Timestamp.UnixMilli() < sinceMS {
				return nil
			}
			m.ConversationID = c.SourceID
			// 3. The same upsert path as the live stream. An unchanged row
			// writes nothing at all, which is what makes running this more
			// often free of side effects.
			if _, err := in.IngestMessage(ctx, m, isDM, false); err != nil {
				return err
			}
		}
		if next == nil {
			return nil
		}
		cursor = next
	}
}

// SweepSince returns the `since` the timer trigger uses: THIS account's
// last_sweep_at_ms (spec section 5.4's table). Every other trigger uses
// last_event_at_ms, which is LastEventAt.
func (a *Account) SweepSince(ctx context.Context) (time.Time, error) {
	row, err := a.Store.Account(ctx, a.ID)
	if err != nil {
		return time.Time{}, err
	}
	return time.UnixMilli(row.LastSweepAtMS).UTC(), nil
}

// SweepOnTimer is the per-account timer trigger, every
// settings.ingest.sweep_interval, with that account's last_sweep_at_ms as
// `since`.
func (a *Account) SweepOnTimer(ctx context.Context) error {
	since, err := a.SweepSince(ctx)
	if err != nil {
		return err
	}
	return a.Reconcile(ctx, since)
}

// SweepOnEvent is every event-driven trigger of section 5.4's table --
// BROWSER_ACTIVE with a changed session ID, MOBILE_DATABASE_SYNC_COMPLETE,
// events.NoDataReceived, events.ListenRecovered and every successful Connect,
// and POST /v1/admin/backfill naming this account -- all of which hand it
// last_event_at_ms.
func (a *Account) SweepOnEvent(ctx context.Context) error {
	since, err := a.LastEventAt(ctx)
	if err != nil {
		return err
	}
	return a.Reconcile(ctx, since)
}

// SweepFromEpoch is POST /v1/admin/backfill with no body: a full re-backfill
// of this account, `since` the epoch. It is the most expensive operation
// Agent GM offers, which is why the route requires {"confirm": true}.
func (a *Account) SweepFromEpoch(ctx context.Context) error {
	return a.Reconcile(ctx, time.UnixMilli(0).UTC())
}
