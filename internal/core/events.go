package core

import (
	"context"
	"github.com/thisnick/agent-gm/internal/gm"
	"github.com/thisnick/agent-gm/internal/store"
)

// HandleEvent applies one event from THIS account's stream (spec section
// 3.4). There is one handler per account, driven by one goroutine, and
// nothing here ever touches another account's rows or another account's
// state.
//
// The order inside is deliberate. A trigger that runs a sweep reads
// `last_event_at_ms` BEFORE the event is allowed to move it: the whole value
// of `since` is that it precedes the data the sweep is going to recover, and
// stamping it first would hand the sweep a bound above the very messages the
// library's dedup lost.
func (a *Account) HandleEvent(ctx context.Context, ev gm.Event) error {
	in := a.Ingest()

	switch e := ev.(type) {
	case *gm.EventClientReady:
		a.setSessionID(e.SessionID)
		for _, c := range e.Conversations {
			if _, err := in.IngestConversation(ctx, c); err != nil {
				return err
			}
		}
		// Every successful Connect sweeps, since `last_event_at_ms`, and then
		// kicks this account's backfill. The backfill runs on its own
		// goroutine so a MOBILE_DATABASE_SYNC_STARTED alert can still reach
		// this loop and pause it.
		if err := a.SweepOnEvent(ctx); err != nil {
			return err
		}
		a.KickBackfill(ctx, e.Conversations)
		return nil

	case *gm.EventMessage:
		m := e.Message
		conv, err := a.Store.Conversation(ctx, store.ConversationID(a.ID, m.ConversationID))
		isDM := err == nil && !conv.IsGroup
		if _, err := in.IngestMessage(ctx, m, isDM, e.IsOld); err != nil {
			return err
		}
		// An IsOld replay is a replay of data already received; it does not
		// move the moment data was last received.
		if e.IsOld {
			return nil
		}
		return a.TouchEvent(ctx, m.Timestamp)

	case *gm.EventConversation:
		if _, err := in.IngestConversation(ctx, e.Conversation); err != nil {
			return err
		}
		return a.TouchEvent(ctx, e.Conversation.LastActivity)

	case *gm.EventUserAlert:
		return a.handleAlert(ctx, e.Alert)

	case *gm.EventNoDataReceived:
		// Nothing received for dataReceiveCheckInterval. Exactly the shape of
		// loss the sweep exists for.
		return a.SweepOnEvent(ctx)

	case *gm.EventListenRecovered:
		return a.SweepOnEvent(ctx)

	case *gm.EventUnknown:
		in.CountUnknown()
		return nil
	}
	// Everything else in section 3.4's catalogue is the supervisor's
	// business: connection state, pairing, settings and typing are account
	// lifecycle, not ingestion. They are not counted as unknown, because they
	// are named events this loop deliberately does not act on.
	return nil
}

func (a *Account) handleAlert(ctx context.Context, alert gm.AlertType) error {
	switch {
	case alert.TriggersResync():
		// BROWSER_ACTIVE means "our session changed, resync" -- not "another
		// device took over". The comparison is against the session ID last
		// seen; an unchanged one is this session reasserting itself and needs
		// no sweep.
		current := a.Backend.SessionID()
		if current != "" && current == a.LastSessionID() {
			return nil
		}
		a.setSessionID(current)
		return a.SweepOnEvent(ctx)

	case alert.PausesBackfill():
		// THIS phone is resyncing its own database; defer THIS account's
		// backfill only. Other accounts keep going.
		a.PauseBackfill()
		return nil

	case alert == gm.AlertMobileDatabaseSyncComplete:
		a.ResumeBackfill()
		return a.SweepOnEvent(ctx)

	case alert.MarksSessionIdle():
		// A health flag; a BROWSER_ACTIVE resync is expected next.
		return nil
	}
	return nil
}

func (a *Account) setSessionID(id string) {
	if id == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.lastSessionID = id
}

// auditSource is the section 12.3 client source stamped on audit rows this
// engine writes. Ingestion is not a client, so it says so rather than
// borrowing a caller's source.
func (a *Account) auditSource() string {
	if a.Source != "" {
		return a.Source
	}
	return "system"
}
