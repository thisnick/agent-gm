package core_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/thisnick/agent-gm/internal/core"
	"github.com/thisnick/agent-gm/internal/gm"
	"github.com/thisnick/agent-gm/internal/store"
)

func TestFreshReadsCoalesceAndDiscoverPhoneRename(t *testing.T) {
	h := newHarness(t)
	f := &core.Freshener{}
	h.Account.Freshness = f
	c := h.seedConversation(convA, false)
	c.Name = "New contact name"
	c.Participants[1].DisplayName = c.Name
	h.Backend.SeedConversation(c)
	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := f.Refresh(context.Background(), h.Account, "conversations", false); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	// Inbox and archive, once each for all callers.
	if got := h.Backend.CallCount("ListConversations"); got != 2 {
		t.Fatalf("duplicate refreshes: %d", got)
	}
	rows, err := h.Store.ListConversations(h.ctx(), store.ConversationQuery{AccountID: h.Account.ID, ParticipantPhone: fictionalA})
	if err != nil || len(rows) != 1 || rows[0].Name != c.Name {
		t.Fatalf("rename/phone lookup: %+v %v", rows, err)
	}
	h.Clock.Advance(61 * time.Second)
	if err = f.Refresh(h.ctx(), h.Account, "conversations", false); err != nil {
		t.Fatal(err)
	}
	if got := h.Backend.CallCount("ListConversations"); got != 4 {
		t.Fatalf("stale read did not refresh: %d", got)
	}
	// A new coordinator after restart still uses the persisted freshness record.
	if err = (&core.Freshener{}).Refresh(h.ctx(), h.Account, "conversations", false); err != nil {
		t.Fatal(err)
	}
	if got := h.Backend.CallCount("ListConversations"); got != 4 {
		t.Fatal("freshness lost across coordinator restart")
	}
}

func TestThreadRefreshFailureKeepsCheckpointAndRetriesWithoutDuplicates(t *testing.T) {
	h := newHarness(t)
	f := &core.Freshener{}
	h.Account.Freshness = f
	c := h.seedConversation(convA, false)
	id := store.ConversationID(h.Account.ID, c.SourceID)
	h.Account.Config.MessagePageSize = 2
	cutoff := h.Clock.Now().Add(-3 * time.Minute).UnixMilli()
	if err := h.Store.SetAccountLastSweep(h.ctx(), h.Account.ID, time.UnixMilli(cutoff)); err != nil {
		t.Fatal(err)
	}
	for i := range 5 {
		h.Backend.SeedMessage(message(convA, i, h.Clock.Now().Add(-time.Duration(i)*time.Minute)))
	}
	h.Backend.ScriptErrors(nil, nil, errors.New("second page failed"))
	if err := f.Refresh(h.ctx(), h.Account, id, false); err == nil {
		t.Fatal("expected failed page")
	}
	at, err := h.Store.LastRefresh(h.ctx(), h.Account.ID, id)
	if err != nil || at != 0 {
		t.Fatalf("advanced failed refresh: %d %v", at, err)
	}
	if err = f.Refresh(h.ctx(), h.Account, id, false); err != nil {
		t.Fatal(err)
	}
	if got := h.countMessages(id); got != 4 {
		t.Fatalf("timestamp cutoff/dedup: %d messages, want 4", got)
	}
	at, err = h.Store.LastRefresh(h.ctx(), h.Account.ID, id)
	if err != nil || at == 0 {
		t.Fatal("successful refresh not recorded")
	}
}

func TestSendAlwaysRefreshesDestinationAndDoesNotSendOnRefreshFailure(t *testing.T) {
	h := newHarness(t)
	h.Account.Freshness = &core.Freshener{}
	c := h.seedConversation(convA, false)
	c.Name = "Renamed before sending"
	h.Backend.SeedConversation(c)
	for _, key := range []string{"fresh-send-1", "fresh-send-2"} {
		if _, err := h.Account.SendText(h.ctx(), sendInput(h, key, "test")); err != nil {
			t.Fatal(err)
		}
	}
	if h.Backend.CallCount("GetConversation") != 2 || h.Backend.CallCount("SendText") != 2 {
		t.Fatal("send skipped mandatory refresh")
	}
	h.Backend.ScriptErrors(errors.New("phone unavailable"))
	if _, err := h.Account.SendText(h.ctx(), sendInput(h, "fresh-send-3", "test")); err == nil {
		t.Fatal("send continued after failed refresh")
	}
	if h.Backend.CallCount("SendText") != 2 {
		t.Fatal("sent despite refresh failure")
	}
	if _, err := h.Account.SendText(h.ctx(), sendInput(h, "fresh-send-1", "test")); err != nil {
		t.Fatal(err)
	}
	if h.Backend.CallCount("SendText") != 2 {
		t.Fatal("idempotent retry sent twice")
	}
}

type slowRefreshBackend struct {
	gm.Backend
	advance func()
}

func (b slowRefreshBackend) WithSession(ctx context.Context, fn func(context.Context) error) error {
	err := fn(ctx)
	b.advance()
	return err
}

func TestSlowRefreshKeepsStartCutoffAndCachesFromCompletion(t *testing.T) {
	h := newHarness(t)
	h.seedConversation(convA, false)
	f := &core.Freshener{}
	started := h.Clock.Now().UnixMilli()
	h.Account.Backend = slowRefreshBackend{Backend: h.Backend, advance: func() { h.Clock.Advance(2 * time.Minute) }}
	if err := f.Refresh(h.ctx(), h.Account, "conversations", false); err != nil {
		t.Fatal(err)
	}
	since, err := h.Store.RefreshSince(h.ctx(), h.Account.ID, "conversations")
	if err != nil || since != started {
		t.Fatalf("cutoff = %d, want %d: %v", since, started, err)
	}
	completed, err := h.Store.LastRefresh(h.ctx(), h.Account.ID, "conversations")
	if err != nil || completed != h.Clock.Now().UnixMilli() {
		t.Fatalf("completion = %d: %v", completed, err)
	}
	if err := f.Refresh(h.ctx(), h.Account, "conversations", false); err != nil {
		t.Fatal(err)
	}
	if got := h.Backend.CallCount("ListConversations"); got != 2 {
		t.Fatalf("slow refresh immediately stale: %d", got)
	}
}

func TestFreshnessPreservesExplicitEpochBackfill(t *testing.T) {
	h := newHarness(t)
	h.Account.Freshness = &core.Freshener{}
	c := h.seedConversation(convA, false)
	if err := h.Account.SweepOnTimer(h.ctx()); err != nil {
		t.Fatal(err)
	}
	h.Backend.SeedMessage(message(convA, 1, h.Clock.Now().Add(-24*time.Hour)))
	if err := h.Account.SweepFromEpoch(h.ctx()); err != nil {
		t.Fatal(err)
	}
	if got := h.countMessages(store.ConversationID(h.Account.ID, c.SourceID)); got != 1 {
		t.Fatalf("epoch backfill skipped old message: %d", got)
	}
}
