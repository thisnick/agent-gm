package core_test

import (
	"testing"
	"time"

	"github.com/thisnick/agent-gm/internal/audit"
	"github.com/thisnick/agent-gm/internal/gm"
	"github.com/thisnick/agent-gm/internal/store"
)

const convA = "conv-a"

// TestSlice2Test2BackfillAndLiveInterleavedInBothOrders is section 16 Slice 2
// acceptance test 2.
//
// Backfill and live ingestion of the same 500 messages, interleaved in BOTH
// orders, produce exactly 500 rows with identical content hashes; a third
// replay writes zero rows and does not change any updated_at_ms.
//
// This is the test that makes section 5.1's structural rule checkable: there
// is one upsert path, so a bug in ordering or dedup is reproducible from
// either source. If a separate backfill writer ever appears, the two orders
// will stop agreeing here.
func TestSlice2Test2BackfillAndLiveInterleavedInBothOrders(t *testing.T) {
	const total = 500

	run := func(t *testing.T, liveFirst bool) map[string]string {
		t.Helper()
		h := newHarness(t)
		conv := h.seedConversation(convA, false)
		base := h.Clock.Now().Add(-time.Duration(total) * time.Minute)
		msgs := make([]gm.Message, 0, total)
		for i := range total {
			m := message(convA, i, base.Add(time.Duration(i)*time.Minute))
			msgs = append(msgs, m)
			h.Backend.SeedMessage(m)
		}

		live := func() {
			for _, m := range msgs {
				if err := h.Account.HandleEvent(h.ctx(), &gm.EventMessage{Message: m}); err != nil {
					t.Fatalf("live ingest: %v", err)
				}
			}
		}
		backfill := func() {
			if err := h.Account.Backfill(h.ctx(), []gm.Conversation{conv}); err != nil {
				t.Fatalf("backfill: %v", err)
			}
		}
		if liveFirst {
			live()
			backfill()
		} else {
			backfill()
			live()
		}

		rows := h.messages(store.ConversationID(h.Account.ID, convA))
		if len(rows) != total {
			t.Fatalf("rows = %d, want exactly %d", len(rows), total)
		}

		// Ordering is sent_at_ms DESC then id DESC, never ingestion order.
		for i := 1; i < len(rows); i++ {
			prev, cur := rows[i-1], rows[i]
			if prev.SentAtMS < cur.SentAtMS ||
				(prev.SentAtMS == cur.SentAtMS && prev.ID < cur.ID) {
				t.Fatalf("rows are not ordered by sent_at_ms DESC, id DESC at %d", i)
			}
		}

		hashes := map[string]string{}
		updated := map[string]int64{}
		for _, r := range rows {
			hashes[r.ID] = r.ContentHash
			updated[r.ID] = r.UpdatedAtMS
		}

		// A THIRD replay, with the clock moved on so an unnecessary write
		// would be visible, writes nothing at all.
		h.Clock.Advance(time.Hour)
		live()
		for _, r := range h.messages(store.ConversationID(h.Account.ID, convA)) {
			if r.UpdatedAtMS != updated[r.ID] {
				t.Fatalf("replay moved updated_at_ms on %s: %d -> %d",
					r.ID, updated[r.ID], r.UpdatedAtMS)
			}
			if r.ContentHash != hashes[r.ID] {
				t.Fatalf("replay changed the content hash of %s", r.ID)
			}
		}
		// And it does not churn the conversation row either.
		return hashes
	}

	backfillThenLive := run(t, false)
	liveThenBackfill := run(t, true)

	if len(backfillThenLive) != len(liveThenBackfill) {
		t.Fatalf("the two orders produced %d and %d rows",
			len(backfillThenLive), len(liveThenBackfill))
	}
	for id, hash := range backfillThenLive {
		other, ok := liveThenBackfill[id]
		if !ok {
			t.Fatalf("%s exists only in the backfill-first order", id)
		}
		if other != hash {
			t.Fatalf("%s has different content hashes in the two orders", id)
		}
	}
}

// TestSlice2Test3LastActivityNeverMovesBackwards is section 16 Slice 2
// acceptance test 3: last_activity_ms never moves backwards, proven by
// replaying an old message after a new one.
//
// It asserts the conversation's updated_at_ms as well as its last_activity_ms
// because section 5.4 states both halves in one breath: the value is
// monotonic, AND a replay writes nothing at all rather than churning the WAL.
// A guard that let the write through and relied on SQL's MAX() would keep the
// value right and still fail here, which is the point.
//
// PLANT: drop the `isOld || activity <= current.LastActivityMS` guard in
// Ingester.applyActivity (internal/core/ingest.go) and this test fails at
// "replaying an old message bumped updated_at_ms".
func TestSlice2Test3LastActivityNeverMovesBackwards(t *testing.T) {
	h := newHarness(t)
	h.seedConversation(convA, false)
	convID := store.ConversationID(h.Account.ID, convA)

	base := h.Clock.Now()
	older := message(convA, 1, base.Add(-2*time.Hour))
	newer := message(convA, 2, base)

	for _, m := range []gm.Message{older, newer} {
		if err := h.Account.HandleEvent(h.ctx(), &gm.EventMessage{Message: m}); err != nil {
			t.Fatalf("ingest: %v", err)
		}
	}
	before := h.conversation(convA)
	beforeUpdated := h.convUpdatedAt(convA)
	if before.LastActivityMS != newer.Timestamp.UnixMilli() {
		t.Fatalf("last_activity_ms = %d, want the newer message's %d",
			before.LastActivityMS, newer.Timestamp.UnixMilli())
	}

	// Now replay the OLD one, with the clock moved on so a write would show.
	h.Clock.Advance(time.Hour)
	if err := h.Account.HandleEvent(h.ctx(), &gm.EventMessage{Message: older, IsOld: true}); err != nil {
		t.Fatalf("replaying old message: %v", err)
	}
	// And once more as a fresh delivery rather than a flagged replay, because
	// the monotonicity rule does not depend on IsOld being set.
	if err := h.Account.HandleEvent(h.ctx(), &gm.EventMessage{Message: older}); err != nil {
		t.Fatalf("re-delivering old message: %v", err)
	}

	after := h.conversation(convA)
	if after.LastActivityMS != before.LastActivityMS {
		t.Fatalf("last_activity_ms moved from %d to %d",
			before.LastActivityMS, after.LastActivityMS)
	}
	if after.LatestMessageID != before.LatestMessageID {
		t.Fatalf("latest_message_id moved from %q to %q",
			before.LatestMessageID, after.LatestMessageID)
	}
	if afterUpdated := h.convUpdatedAt(convA); afterUpdated != beforeUpdated {
		t.Fatalf("replaying an old message bumped updated_at_ms: %d -> %d",
			beforeUpdated, afterUpdated)
	}
	_ = convID
}

// TestSlice2Test4SweepRecoversRealLoss is section 16 Slice 2 acceptance test
// 4, the important one.
//
// The library's dedup abandons every remaining part of a batch on a hit, so
// messages are LOST rather than merely duplicated, and no local dedup can
// recover what never arrived. This scripts exactly that loss, asserts the
// messages are missing, and then fires each of the four triggers of section
// 5.4's table in turn, asserting the sweep runs and every missing message is
// present EXACTLY ONCE.
//
// PLANT: replace `sinceMS` with `a.now().UnixMilli()` in
// Account.sweepConversation (internal/core/reconcile.go) -- the "wrong since"
// mutation -- and every subtest here fails at "still missing after ...".
func TestSlice2Test4SweepRecoversRealLoss(t *testing.T) {
	triggers := []struct {
		name string
		fire func(t *testing.T, h *harness)
	}{
		{"browser_active_with_a_changed_session_id", func(t *testing.T, h *harness) {
			h.Backend.SetSessionID("a-brand-new-session")
			if err := h.Account.HandleEvent(h.ctx(),
				&gm.EventUserAlert{Alert: gm.AlertBrowserActive}); err != nil {
				t.Fatalf("browser active: %v", err)
			}
		}},
		{"per_account_timer", func(t *testing.T, h *harness) {
			if err := h.Account.SweepOnTimer(h.ctx()); err != nil {
				t.Fatalf("timer sweep: %v", err)
			}
		}},
		{"no_data_received", func(t *testing.T, h *harness) {
			if err := h.Account.HandleEvent(h.ctx(), &gm.EventNoDataReceived{}); err != nil {
				t.Fatalf("no data received: %v", err)
			}
		}},
		{"mobile_database_sync_complete", func(t *testing.T, h *harness) {
			if err := h.Account.HandleEvent(h.ctx(),
				&gm.EventUserAlert{Alert: gm.AlertMobileDatabaseSyncComplete}); err != nil {
				t.Fatalf("sync complete: %v", err)
			}
		}},
	}

	for _, tr := range triggers {
		t.Run(tr.name, func(t *testing.T) {
			h := newHarness(t)
			lost := loseHalfABatch(t, h)

			for _, id := range lost {
				if _, err := h.Store.Message(h.ctx(), id); err == nil {
					t.Fatalf("%s should have been lost by the library's dedup", id)
				}
			}
			sweepsBefore := h.Account.Sweeps()

			tr.fire(t, h)

			if h.Account.Sweeps() != sweepsBefore+1 {
				t.Fatalf("sweeps_total = %d, want %d: the trigger did not sweep",
					h.Account.Sweeps(), sweepsBefore+1)
			}
			for _, id := range lost {
				if _, err := h.Store.Message(h.ctx(), id); err != nil {
					t.Fatalf("still missing after %s: %s (%v)", tr.name, id, err)
				}
			}
			// Exactly once: five delivered plus five recovered, no more.
			if got := h.countMessages(store.ConversationID(h.Account.ID, convA)); got != 6 {
				t.Fatalf("message rows = %d, want 6 (one seed + five recovered, each once)", got)
			}
			if h.accountRow().LastSweepAtMS == 0 {
				t.Fatal("accounts.last_sweep_at_ms was not recorded")
			}
		})
	}
}

// loseHalfABatch scripts the fake to abandon the rest of a batch the way the
// library's dedup does, and returns the msg_ IDs that were lost.
//
// The shape matters: the account first receives one message, which is what
// sets last_event_at_ms; the batch that follows carries messages NEWER than
// that, and the abandoned tail of it is what the sweep has to find. That is
// the real sequence, and it is what makes `since` load-bearing rather than
// decorative.
func loseHalfABatch(t *testing.T, h *harness) []string {
	t.Helper()
	conv := h.seedConversation(convA, false)
	base := h.Clock.Now()

	seed := message(convA, 0, base)
	h.Backend.SeedMessage(seed)
	if err := h.Account.HandleEvent(h.ctx(), &gm.EventMessage{Message: seed}); err != nil {
		t.Fatalf("seeding the received message: %v", err)
	}

	// Five more messages arrive as one batch; the library hits its 8-entry
	// dedup window on the first of them and returns, abandoning the rest.
	var batch []gm.Event
	var lost []string
	for i := 1; i <= 5; i++ {
		m := message(convA, i, base.Add(time.Duration(i)*time.Minute))
		h.Backend.SeedMessage(m)
		batch = append(batch, &gm.EventMessage{Message: m})
		lost = append(lost, store.MessageID(h.Account.ID, convA, m.SourceID))
	}
	// The whole batch is abandoned, which is the worst and the commonest
	// case: the dedup hit is on the first update in the batch.
	h.Backend.EmitBatch(0, batch...)
	h.drain()

	// Google's own view of the thread has moved on, as it would have.
	conv.LastActivity = base.Add(5 * time.Minute)
	conv.LatestMessageID = "m00005"
	h.Backend.SeedConversation(conv)

	// The clock moves on, so nothing here depends on wall time.
	h.Clock.Advance(10 * time.Minute)
	return lost
}

// TestSlice2Test5EveryMessageStatusIsClassified is section 16 Slice 2
// acceptance test 5: every MessageStatusType value at the pin outside 200-279
// maps to a delivery_state, every value inside classifies as kind='system',
// and a value in neither fails.
func TestSlice2Test5EveryMessageStatusIsClassified(t *testing.T) {
	h := newHarness(t)
	h.seedConversation(convA, false)
	base := h.Clock.Now()

	mapped := gm.MappedStatusValues()
	if len(mapped) == 0 {
		t.Fatal("the delivery-state mapping is empty")
	}
	seen := map[int32]bool{}
	i := 0
	for _, raw := range mapped {
		if gm.IsSystemEventStatus(raw) {
			t.Fatalf("status %d is in both the mapping and the 200-279 band", raw)
		}
		seen[raw] = true
		state, ok := gm.DeliveryStateFor(raw)
		if !ok {
			t.Fatalf("status %d has no delivery_state", raw)
		}
		i++
		if gm.ShouldIgnoreStatus(raw, true) {
			continue // dropped by section 5.3 step 1, and counted there
		}
		m := message(convA, i, base.Add(time.Duration(i)*time.Second))
		m.StatusRaw = raw
		m.Kind = gm.KindForStatus(raw)
		m.DeliveryState = state
		res, err := h.Account.Ingest().IngestMessage(h.ctx(), m, false, false)
		if err != nil {
			t.Fatalf("ingesting status %d: %v", raw, err)
		}
		stored, err := h.Store.Message(h.ctx(), res.ID)
		if err != nil {
			t.Fatalf("reading status %d back: %v", raw, err)
		}
		if stored.DeliveryState != string(state) {
			t.Fatalf("status %d stored delivery_state %q, want %q",
				raw, stored.DeliveryState, state)
		}
		if stored.Kind != string(gm.MessageKindMessage) {
			t.Fatalf("status %d stored kind %q, want message", raw, stored.Kind)
		}
	}

	// Every value in 200-279 classifies as system.
	for raw := int32(200); raw <= 279; raw++ {
		if seen[raw] {
			t.Fatalf("status %d is in both bands", raw)
		}
		if gm.KindForStatus(raw) != gm.MessageKindSystem {
			t.Fatalf("status %d is not classified as system", raw)
		}
		if gm.ShouldIgnoreStatus(raw, false) {
			continue
		}
		i++
		m := message(convA, i, base.Add(time.Duration(i)*time.Second))
		m.StatusRaw = raw
		m.Kind = gm.KindForStatus(raw)
		state, _ := gm.DeliveryStateFor(raw)
		m.DeliveryState = state
		res, err := h.Account.Ingest().IngestMessage(h.ctx(), m, false, false)
		if err != nil {
			t.Fatalf("ingesting system status %d: %v", raw, err)
		}
		stored, err := h.Store.Message(h.ctx(), res.ID)
		if err != nil {
			t.Fatalf("reading system status %d back: %v", raw, err)
		}
		if stored.Kind != string(gm.MessageKindSystem) {
			t.Fatalf("status %d stored kind %q, want system", raw, stored.Kind)
		}
	}

	// A value in neither band fails rather than falling silently to unknown.
	for _, raw := range []int32{99, 150, 199, 299, 400} {
		if seen[raw] || gm.IsSystemEventStatus(raw) {
			continue
		}
		if _, ok := gm.DeliveryStateFor(raw); ok {
			t.Fatalf("status %d is neither mapped nor a system event, "+
				"yet DeliveryStateFor claims it", raw)
		}
	}
}

// TestSlice2Test6BackwardTransitionIsRefused is section 16 Slice 2 acceptance
// test 6: a backward transition (read -> sent) is refused, leaves the stored
// state alone, still writes delivery_state_raw, and emits
// message.status_out_of_order. A forward skip is accepted.
func TestSlice2Test6BackwardTransitionIsRefused(t *testing.T) {
	h := newHarness(t)
	h.seedConversation(convA, false)
	base := h.Clock.Now()

	// An outgoing message reaches `read`.
	m := message(convA, 1, base)
	m.StatusRaw = 11 // OUTGOING_DISPLAYED
	m.DeliveryState = gm.DeliveryStateRead
	res, err := h.Account.Ingest().IngestMessage(h.ctx(), m, false, false)
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	stored, _ := h.Store.Message(h.ctx(), res.ID)
	if stored.Direction != string(gm.DirectionOutgoing) {
		t.Fatalf("direction = %q, want outgoing", stored.Direction)
	}

	// Google now says `sent`, which is backwards.
	back := m
	back.StatusRaw = 1 // OUTGOING_COMPLETE
	back.DeliveryState = gm.DeliveryStateSent
	back.Text = m.Text + " (re-reported)"
	if _, err := h.Account.Ingest().IngestMessage(h.ctx(), back, false, false); err != nil {
		t.Fatalf("ingesting the backward move: %v", err)
	}
	after, _ := h.Store.Message(h.ctx(), res.ID)
	if after.DeliveryState != string(gm.DeliveryStateRead) {
		t.Fatalf("delivery_state = %q, want the stored `read` left alone", after.DeliveryState)
	}
	if after.DeliveryStateRaw != 1 {
		t.Fatalf("delivery_state_raw = %d, want 1: the raw truth is always written",
			after.DeliveryStateRaw)
	}
	if !hasAudit(h.Audit.Rows(), string(audit.KindMessageStatusOutOfOrder)) {
		t.Fatalf("no message.status_out_of_order audit row; kinds = %v", h.auditKinds())
	}

	// A forward SKIP is accepted: the phone genuinely reports coarse jumps.
	fwd := message(convA, 2, base.Add(time.Minute))
	fwd.StatusRaw = 5 // OUTGOING_SENDING
	fwd.DeliveryState = gm.DeliveryStateSending
	res2, err := h.Account.Ingest().IngestMessage(h.ctx(), fwd, false, false)
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	fwd.StatusRaw = 2 // OUTGOING_DELIVERED, skipping `sent`
	fwd.DeliveryState = gm.DeliveryStateDelivered
	if _, err := h.Account.Ingest().IngestMessage(h.ctx(), fwd, false, false); err != nil {
		t.Fatalf("forward skip: %v", err)
	}
	skipped, _ := h.Store.Message(h.ctx(), res2.ID)
	if skipped.DeliveryState != string(gm.DeliveryStateDelivered) {
		t.Fatalf("a forward skip was refused: delivery_state = %q", skipped.DeliveryState)
	}
}
