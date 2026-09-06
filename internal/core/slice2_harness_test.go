package core_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/thisnick/agent-gm/internal/audit"
	"github.com/thisnick/agent-gm/internal/clock"
	"github.com/thisnick/agent-gm/internal/core"
	"github.com/thisnick/agent-gm/internal/gm"
	"github.com/thisnick/agent-gm/internal/gm/fake"
	"github.com/thisnick/agent-gm/internal/store"
)

// The harness for section 16 Slice 2's acceptance tests. Nothing in this file
// needs a phone, a network or Docker, which is why none of these tests is
// behind a gate.
//
// ONE FAKE IS ONE ACCOUNT (spec section 13.1). A test wanting two accounts
// builds two harnesses over one store, exactly as production registers two
// backends with internal/accounts. The fake never models several accounts
// internally, so the multi-account paths above `gm` are tested against the
// shape production has.

// Fictional numbers only. +1 202 555 01xx is reserved for fiction; no real
// number appears anywhere in this repository.
const (
	fictionalA = "+12025550101"
	fictionalB = "+12025550102"
)

type harness struct {
	t       *testing.T
	Store   *store.Store
	Clock   *clock.Fake
	Backend *fake.Backend
	Account *core.Account
	Audit   *audit.MemoryAppender
	Dir     string
}

// newHarness builds one account over a fresh store.
func newHarness(t *testing.T) *harness {
	t.Helper()
	return newHarnessIn(t, t.TempDir(), "owner-a@example.test", nil)
}

// newHarnessIn builds one account over the store in dir, so a "restart" is a
// second harness over the same directory.
func newHarnessIn(t *testing.T, dir, address string, clk *clock.Fake) *harness {
	t.Helper()
	if clk == nil {
		clk = clock.NewFake()
	}
	st, err := store.Open(dir, clk)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return attach(t, st, clk, address, dir)
}

// attachAccount registers a SECOND account on an existing store, with its own
// fake and its own clock. Two accounts, one store, no shared state above the
// database -- which is the shape the cross-talk assertions of section 13.2
// need.
func attachAccount(t *testing.T, h *harness, address string) *harness {
	t.Helper()
	return attach(t, h.Store, clock.NewFake(), address, h.Dir)
}

func attach(t *testing.T, st *store.Store, clk *clock.Fake, address, dir string) *harness {
	t.Helper()
	ctx := context.Background()
	id := store.AccountID(address)
	if err := st.UpsertAccount(ctx, store.Account{
		ID:             id,
		GoogleAccount:  address,
		State:          store.StateConnected,
		SessionPresent: true,
		PairedAtMS:     clk.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("creating account row: %v", err)
	}
	appender := audit.NewMemoryAppender()
	be := fake.New(address, fake.WithClock(clk))
	h := &harness{
		t:       t,
		Store:   st,
		Clock:   clk,
		Backend: be,
		Audit:   appender,
		Dir:     dir,
		Account: &core.Account{
			ID:      id,
			Store:   st,
			Backend: be,
			Clock:   clk,
			Config:  core.DefaultConfig(),
			Audit:   audit.NewWriter(appender, clk),
			Source:  "test",
		},
	}
	return h
}

func (h *harness) ctx() context.Context { return context.Background() }

// seedConversation puts one thread on the fake and returns its source ID.
func (h *harness) seedConversation(sourceID string, group bool) gm.Conversation {
	c := gm.Conversation{
		SourceID:          sourceID,
		Name:              sourceID,
		IsGroup:           group,
		Type:              gm.ConversationTypeRCS,
		SendModeRaw:       gm.SendModeAuto,
		Folder:            gm.FolderInbox,
		DefaultOutgoingID: "me@" + h.Backend.Address(),
		LastActivity:      h.Clock.Now(),
		Participants: []gm.Participant{
			{SourceID: "me@" + h.Backend.Address(), IsMe: true, IsVisible: true},
			{SourceID: "part-a", PhoneE164: fictionalA, IsVisible: true},
		},
	}
	h.Backend.SeedConversation(c)
	// The store needs the thread before any of its messages: a message row
	// carries a foreign key onto its conversation. Backfill and the sweep
	// both upsert conversations before fetching messages for exactly that
	// reason, and a test that seeds only the fake would be testing a shape
	// production never has.
	if _, err := h.Account.Ingest().IngestConversation(h.ctx(), c); err != nil {
		h.t.Fatalf("seeding conversation: %v", err)
	}
	return c
}

// message builds one incoming message with a sortable source ID.
func message(convID string, i int, at time.Time) gm.Message {
	return gm.Message{
		SourceID:       fmt.Sprintf("m%05d", i),
		ConversationID: convID,
		ParticipantID:  "part-a",
		Text:           fmt.Sprintf("message %d", i),
		Timestamp:      at,
		StatusRaw:      100, // INCOMING_COMPLETE
		Kind:           gm.MessageKindMessage,
		DeliveryState:  gm.DeliveryStateReceived,
	}
}

func (h *harness) countMessages(convID string) int {
	h.t.Helper()
	var n int
	if err := h.Store.Reader().QueryRowContext(h.ctx(),
		`SELECT COUNT(*) FROM messages WHERE account_id = ? AND conversation_id = ?`,
		h.Account.ID, convID).Scan(&n); err != nil {
		h.t.Fatalf("counting messages: %v", err)
	}
	return n
}

// messages pages the whole thread through the real query layer, which caps a
// page at store.MaxLimit. Paging it rather than asking for one huge page is
// deliberate: it exercises the cursor over hundreds of rows, which is where
// the sent_at_ms DESC, id DESC ordering has to hold.
func (h *harness) messages(convID string) []store.Message {
	h.t.Helper()
	var (
		out    []store.Message
		cursor *store.Cursor
	)
	for {
		page, err := h.Store.ListMessages(h.ctx(), store.MessageQuery{
			AccountID:      h.Account.ID,
			ConversationID: convID,
			Limit:          store.MaxLimit,
			IncludeSystem:  true,
			Cursor:         cursor,
		})
		if err != nil {
			h.t.Fatalf("listing messages: %v", err)
		}
		out = append(out, page...)
		if len(page) < store.MaxLimit {
			return out
		}
		c := page[len(page)-1].CursorPosition()
		cursor = &c
	}
}

func (h *harness) conversation(sourceID string) store.Conversation {
	h.t.Helper()
	c, err := h.Store.Conversation(h.ctx(), store.ConversationID(h.Account.ID, sourceID))
	if err != nil {
		h.t.Fatalf("reading conversation: %v", err)
	}
	return c
}

// convUpdatedAt reads conversations.updated_at_ms, which store.Conversation
// does not carry. It is read directly because the assertion it supports --
// that a replay writes NOTHING, rather than writing the same value again --
// is about the row's churn and not about its content.
func (h *harness) convUpdatedAt(sourceID string) int64 {
	h.t.Helper()
	var ms int64
	err := h.Store.Reader().QueryRowContext(h.ctx(),
		`SELECT updated_at_ms FROM conversations WHERE id = ?`,
		store.ConversationID(h.Account.ID, sourceID)).Scan(&ms)
	if err != nil {
		h.t.Fatalf("reading conversation updated_at_ms: %v", err)
	}
	return ms
}

func (h *harness) accountRow() store.Account {
	h.t.Helper()
	a, err := h.Store.Account(h.ctx(), h.Account.ID)
	if err != nil {
		h.t.Fatalf("reading account: %v", err)
	}
	return a
}

// drain applies every event the fake has queued, in order, through the real
// event dispatcher.
func (h *harness) drain() {
	h.t.Helper()
	for {
		select {
		case ev := <-h.Backend.Events():
			if err := h.Account.HandleEvent(h.ctx(), ev); err != nil {
				h.t.Fatalf("handling event: %v", err)
			}
		default:
			return
		}
	}
}

func (h *harness) auditKinds() []string {
	h.t.Helper()
	var out []string
	for _, r := range h.Audit.Rows() {
		out = append(out, r.Kind)
	}
	return out
}

func hasAudit(rows []audit.Row, kind string) bool {
	for _, r := range rows {
		if r.Kind == kind {
			return true
		}
	}
	return false
}
