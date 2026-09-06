package core

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/thisnick/agent-gm/internal/gm"
	"github.com/thisnick/agent-gm/internal/store"
)

// Backfill is spec section 5.2, for THIS account, in the order the spec
// numbers.
//
//	1. Upsert the conversations carried on ClientReady.
//	2. ListConversations(INBOX, conversation_page_size) -- which is also what
//	   arms the library's BUGLE_MESSAGE mode (section 3.7).
//	3. ListConversations(ARCHIVE) if backfill.include_archive.
//	4. ListContacts() and ListTopContacts(); upsert contacts; link
//	   participants.
//	5. For each conversation, oldest-activity first, page FetchMessages until
//	   the page is empty, max_messages_per_conversation is reached, or a
//	   message older than backfill.horizon is seen.
//	6. Record per-conversation progress in backfill_state so a restart
//	   resumes rather than restarting.
//	7. Set accounts.backfill_complete_at_ms for THIS account.
//
// Every row written carries this account's account_id, and every call is on
// this account's client. Concurrency is backfill.concurrency conversations at
// a time per account.
//
// The whole of it pauses while a MOBILE_DATABASE_SYNC_STARTED/SYNCING alert
// from THIS phone is outstanding and resumes on that phone's
// MOBILE_DATABASE_SYNC_COMPLETE; one sleepy phone never stalls another
// account, because the flag is on this Account.
func (a *Account) Backfill(ctx context.Context, ready []gm.Conversation) error {
	in := a.Ingest()

	// 1. The conversations carried on ClientReady.
	for _, c := range ready {
		if _, err := in.IngestConversation(ctx, c); err != nil {
			return err
		}
	}

	if err := a.waitForSync(ctx); err != nil {
		return err
	}

	// 2. INBOX. This call also arms the library's BUGLE_MESSAGE mode, and the
	// flag lives on the Client, of which there is one per account -- so
	// satisfying the rule for one account leaves another's conversation
	// events untrustworthy. It is issued on every account's connect.
	convs, err := a.Backend.ListConversations(ctx, gm.FolderInbox, a.Config.ConversationPageSize)
	if err != nil {
		return fmt.Errorf("listing inbox conversations: %w", err)
	}
	// 3. ARCHIVE.
	if a.Config.IncludeArchive {
		arch, err := a.Backend.ListConversations(ctx, gm.FolderArchive, a.Config.ConversationPageSize)
		if err != nil {
			return fmt.Errorf("listing archived conversations: %w", err)
		}
		convs = append(convs, arch...)
	}
	for _, c := range convs {
		if _, err := in.IngestConversation(ctx, c); err != nil {
			return err
		}
	}

	// 4. Contacts and top contacts, then link participants.
	if err := a.backfillContacts(ctx); err != nil {
		return err
	}

	// 5 and 6. Oldest-activity first, so the deepest history is walked while
	// the newest threads are already readable.
	targets := append([]gm.Conversation(nil), convs...)
	targets = append(targets, ready...)
	targets = dedupeConversations(targets)
	sort.SliceStable(targets, func(i, j int) bool {
		if !targets[i].LastActivity.Equal(targets[j].LastActivity) {
			return targets[i].LastActivity.Before(targets[j].LastActivity)
		}
		return targets[i].SourceID < targets[j].SourceID
	})

	if err := a.eachConversation(ctx, targets, func(ctx context.Context, c gm.Conversation) error {
		return a.backfillConversation(ctx, c)
	}); err != nil {
		return err
	}

	// 7. THIS account is complete. Never a server_meta key.
	return a.setBackfillComplete(ctx, a.now())
}

// KickBackfill runs Backfill on its own goroutine, so an account's event loop
// stays free to deliver the MOBILE_DATABASE_SYNC_STARTED alert that pauses it.
// A backfill that ran inline on the event loop could never be told to pause.
func (a *Account) KickBackfill(ctx context.Context, ready []gm.Conversation) {
	a.backfillWG.Add(1)
	go func() {
		defer a.backfillWG.Done()
		if err := a.Backfill(ctx, ready); err != nil && a.Log != nil {
			a.Log.Warn("backfill failed", "account_id", a.ID, "error", err.Error())
		}
	}()
}

// WaitBackfill blocks until every kicked backfill has finished.
func (a *Account) WaitBackfill() { a.backfillWG.Wait() }

func dedupeConversations(in []gm.Conversation) []gm.Conversation {
	seen := map[string]bool{}
	out := in[:0]
	for _, c := range in {
		if seen[c.SourceID] {
			continue
		}
		seen[c.SourceID] = true
		out = append(out, c)
	}
	return out
}

// eachConversation runs fn over the conversations backfill.concurrency at a
// time, PER ACCOUNT. The worst case across the server is therefore
// backfill.concurrency x accounts.max_concurrent in-flight FetchMessages
// calls, which is section 15.1's scope column made real.
func (a *Account) eachConversation(ctx context.Context, convs []gm.Conversation, fn func(context.Context, gm.Conversation) error) error {
	n := a.Config.BackfillConcurrency
	if n < 1 {
		n = 1
	}
	if n > 8 {
		n = 8
	}
	sem := make(chan struct{}, n)
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		firstEr error
	)
	for _, c := range convs {
		if firstErr(&mu, &firstEr) != nil || ctx.Err() != nil {
			break
		}
		sem <- struct{}{}
		wg.Add(1)
		go func(c gm.Conversation) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := fn(ctx, c); err != nil {
				mu.Lock()
				if firstEr == nil {
					firstEr = err
				}
				mu.Unlock()
			}
		}(c)
	}
	wg.Wait()
	return firstErr(&mu, &firstEr)
}

func firstErr(mu *sync.Mutex, err *error) error {
	mu.Lock()
	defer mu.Unlock()
	return *err
}

func (a *Account) backfillContacts(ctx context.Context) error {
	contacts, err := a.Backend.ListContacts(ctx)
	if err != nil {
		return fmt.Errorf("listing contacts: %w", err)
	}
	top, err := a.Backend.ListTopContacts(ctx)
	if err != nil {
		return fmt.Errorf("listing top contacts: %w", err)
	}
	for _, c := range append(contacts, markTop(top)...) {
		if _, err := a.Store.UpsertContact(ctx, a.ID, c, ""); err != nil {
			return fmt.Errorf("upserting contact: %w", err)
		}
	}
	// Linking participants to contacts is a per-account relink: it matches on
	// phone number within this account's rows and never reaches another
	// account's participants.
	if err := a.Store.RelinkAccountContacts(ctx, a.ID); err != nil {
		return fmt.Errorf("linking participants to contacts: %w", err)
	}
	return nil
}

func markTop(top []gm.Contact) []gm.Contact {
	out := make([]gm.Contact, 0, len(top))
	for _, c := range top {
		c.IsTop = true
		out = append(out, c)
	}
	return out
}

// backfillConversation is step 5 for one thread, resuming from
// backfill_state so a restart continues rather than restarting (step 6).
func (a *Account) backfillConversation(ctx context.Context, c gm.Conversation) error {
	convID := store.ConversationID(a.ID, c.SourceID)
	state, err := a.Store.BackfillState(ctx, convID)
	switch {
	case errors.Is(err, store.ErrBackfillStateNotFound):
		state = store.BackfillState{ConversationID: convID, AccountID: a.ID}
	case err != nil:
		return fmt.Errorf("reading backfill state: %w", err)
	case state.Complete:
		return nil
	}

	var cursor *gm.Cursor
	if state.CursorItemID != "" {
		cursor = &gm.Cursor{
			LastItemID:          state.CursorItemID,
			LastItemTimestampUS: state.CursorTimestampUS,
		}
	}
	horizonMS := a.now().Add(-a.Config.Horizon).UnixMilli()
	isDM := !c.IsGroup
	in := a.Ingest()

	for {
		if err := a.waitForSync(ctx); err != nil {
			return err
		}
		page, next, err := a.Backend.ListMessages(ctx, c.SourceID, a.Config.MessagePageSize, cursor)
		if err != nil {
			return fmt.Errorf("fetching messages: %w", err)
		}
		// The page is empty: this thread is fully walked.
		if len(page) == 0 {
			state.Complete = true
			break
		}
		reachedHorizon := false
		for _, m := range page {
			if !m.Timestamp.IsZero() && m.Timestamp.UnixMilli() < horizonMS {
				// A message older than backfill.horizon. Everything beyond it
				// is older still, because the page is newest-first.
				reachedHorizon = true
				break
			}
			m.ConversationID = c.SourceID
			// Backfilled messages are not replays: they are the authoritative
			// history. They travel the same upsert path as the live stream.
			if _, err := in.IngestMessage(ctx, m, isDM, false); err != nil {
				return err
			}
			state.MessagesDone++
			if state.OldestSeenMS == 0 || m.Timestamp.UnixMilli() < state.OldestSeenMS {
				state.OldestSeenMS = m.Timestamp.UnixMilli()
			}
		}
		if next != nil {
			state.CursorItemID = next.LastItemID
			state.CursorTimestampUS = next.LastItemTimestampUS
		}
		// 6. Progress is recorded after every page, so a restart resumes at
		// the page boundary rather than at the beginning of the thread.
		if err := a.Store.SetBackfillState(ctx, state); err != nil {
			return fmt.Errorf("recording backfill state: %w", err)
		}
		switch {
		case reachedHorizon, next == nil:
			state.Complete = true
		case a.Config.MaxMessagesPerConversation > 0 &&
			state.MessagesDone >= int64(a.Config.MaxMessagesPerConversation):
			state.Complete = true
		}
		if state.Complete {
			break
		}
		cursor = next
	}
	if err := a.Store.SetBackfillState(ctx, state); err != nil {
		return fmt.Errorf("recording backfill state: %w", err)
	}
	return nil
}

// BackfillHorizonAt is the cut-off a backfill would apply now, exposed so a
// caller can report it without recomputing the arithmetic.
func (a *Account) BackfillHorizonAt() time.Time { return a.now().Add(-a.Config.Horizon) }
