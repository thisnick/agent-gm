package store_test

import (
	"context"
	"crypto/sha1"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/thisnick/agent-gm/internal/clock"
	"github.com/thisnick/agent-gm/internal/gm"
	"github.com/thisnick/agent-gm/internal/store"
)

func newStore(t *testing.T) (*store.Store, *clock.Fake) {
	t.Helper()
	clk := clock.NewFake()
	st, err := store.Open(t.TempDir(), clk)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st, clk
}

func seedAccount(t *testing.T, st *store.Store, address string) string {
	t.Helper()
	id := store.AccountID(address)
	if err := st.UpsertAccount(context.Background(), store.Account{
		ID: id, GoogleAccount: address, State: store.StateConnected,
	}); err != nil {
		t.Fatalf("seeding account: %v", err)
	}
	return id
}

// --- identifiers -------------------------------------------------------------

// Spec section 4.1 and D6/D28: account_id is in every root derivation, and the
// derivation is computed here independently -- not by calling the same
// function twice.
func TestIDDerivationComputedIndependently(t *testing.T) {
	ns := uuid.MustParse("5ec9ab28-5363-57f8-b543-27672479b604")
	if store.IDNamespace != ns {
		t.Fatalf("the frozen namespace changed: %s", store.IDNamespace)
	}

	// An independent UUIDv5: SHA-1 over namespace bytes plus the NUL-joined
	// components, with the version and variant bits set.
	independent := func(components ...string) string {
		h := sha1.New()
		h.Write(ns[:])
		h.Write([]byte(strings.Join(components, "\x00")))
		sum := h.Sum(nil)
		var out uuid.UUID
		copy(out[:], sum[:16])
		out[6] = (out[6] & 0x0f) | 0x50 // version 5
		out[8] = (out[8] & 0x3f) | 0x80 // RFC 4122 variant
		return out.String()
	}

	const address = "owner@example.com"
	acct := store.AccountID(address)
	if want := "acct_" + independent("account", address); acct != want {
		t.Errorf("AccountID = %s, want %s", acct, want)
	}
	conv := store.ConversationID(acct, "goog-conv-1")
	if want := "conv_" + independent(acct, "conversation", "goog-conv-1"); conv != want {
		t.Errorf("ConversationID = %s, want %s", conv, want)
	}
	msg := store.MessageID(acct, "goog-conv-1", "goog-msg-1")
	if want := "msg_" + independent(acct, "message", "goog-conv-1", "goog-msg-1"); msg != want {
		t.Errorf("MessageID = %s, want %s", msg, want)
	}
	contact := store.ContactID(acct, "goog-part-1")
	if want := "contact_" + independent(acct, "contact", "goog-part-1"); contact != want {
		t.Errorf("ContactID = %s, want %s", contact, want)
	}
	// att_, react_ and part_ inherit the account through their parent.
	part := store.ParticipantID(conv, "goog-part-1")
	if want := "part_" + independent(conv, "goog-part-1"); part != want {
		t.Errorf("ParticipantID = %s, want %s", part, want)
	}
	att := store.AttachmentID(msg, "0", "goog-media-1")
	if want := "att_" + independent(msg, "0", "goog-media-1"); att != want {
		t.Errorf("AttachmentID = %s, want %s", att, want)
	}
	react := store.ReactionID(msg, part, "❤️")
	if want := "react_" + independent(msg, part, "❤️"); react != want {
		t.Errorf("ReactionID = %s, want %s", react, want)
	}
}

// The direction that matters (D28): the same Google account on a DIFFERENT
// phone produces the SAME IDs, while a different Google account produces
// different ones.
func TestSameAccountDifferentPhoneKeepsEveryID(t *testing.T) {
	const address = "owner@example.com"
	// Re-pairing to a second phone changes DestRegID, PairingID, SessionID
	// and FinishGaiaPairing's return -- none of which is in the derivation.
	first := store.AccountID(address)
	second := store.AccountID(address)
	if first != second {
		t.Fatalf("re-pairing the same account produced %s then %s", first, second)
	}
	if store.ConversationID(first, "c") != store.ConversationID(second, "c") {
		t.Error("a conversation ID changed across a re-pair")
	}
	if store.MessageID(first, "c", "m") != store.MessageID(second, "c", "m") {
		t.Error("a message ID changed across a re-pair")
	}

	// A different account gives a disjoint ID space.
	other := store.AccountID("other@example.com")
	if other == first {
		t.Fatal("two different accounts share an acct_ ID")
	}
	if store.ConversationID(other, "c") == store.ConversationID(first, "c") {
		t.Error("the same Google conversation ID on two accounts collided")
	}
	if store.MessageID(other, "c", "m") == store.MessageID(first, "c", "m") {
		t.Error("the same Google message ID on two accounts collided")
	}

	// The address is lowercased before hashing, so the same account written
	// two ways is one account, not two.
	if store.AccountID("Owner@Example.com") != first {
		t.Error("the same address in a different case produced a second account")
	}
}

func TestHasPrefixRejectsTheWrongType(t *testing.T) {
	acct := store.AccountID("owner@example.com")
	if !store.HasPrefix(acct, store.PrefixAccount) {
		t.Error("an acct_ ID must match its own prefix")
	}
	if store.HasPrefix(acct, store.PrefixConversation) {
		t.Error("an acct_ ID must not match conv_")
	}
	// A raw Google ID is the same mistake as a wrong prefix.
	if store.HasPrefix("goog-conv-1", store.PrefixConversation) {
		t.Error("a raw Google ID must not pass as a conv_ ID")
	}
	if store.HasPrefix("conv_not-a-uuid", store.PrefixConversation) {
		t.Error("a conv_ prefix over a non-UUID must not pass")
	}
	if !store.HasPrefix(store.OperationID(), store.PrefixOperation) {
		t.Error("an op_ ID must match its own prefix")
	}
}

// --- schema ------------------------------------------------------------------

func TestMigrationLeavesNoForeignKeyViolations(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	v, err := st.Version()
	if err != nil {
		t.Fatal(err)
	}
	if v != store.SchemaVersion() {
		t.Errorf("user_version = %d, want %d", v, store.SchemaVersion())
	}
	acct := seedAccount(t, st, "owner@example.com")
	if _, err := st.UpsertConversation(ctx, acct, gm.Conversation{
		SourceID: "c1", Folder: gm.FolderInbox, Type: gm.ConversationTypeRCS,
		SendModeRaw: gm.SendModeAuto, LastActivity: time.Unix(1, 0),
		Participants: []gm.Participant{{SourceID: "p1", PhoneE164: "+12025550123"}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertMessage(ctx, acct, "c1", gm.Message{
		SourceID: "m1", ConversationID: "c1", Timestamp: time.Unix(1, 0),
		StatusRaw: 5, Kind: gm.MessageKindMessage, DeliveryState: gm.DeliveryStateSending,
	}); err != nil {
		t.Fatal(err)
	}
	bad, err := st.ForeignKeyCheck(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(bad) != 0 {
		t.Errorf("PRAGMA foreign_key_check is not empty: %v", bad)
	}
}

// A database at a higher user_version than the binary knows refuses to open,
// naming both numbers. There is no down-migration.
func TestDatabaseFromTheFutureRefusesToOpen(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir, clock.NewFake())
	if err != nil {
		t.Fatal(err)
	}
	_ = st.Close()

	db, err := sql.Open("sqlite", filepath.Join(dir, "agent-gm.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	future := store.SchemaVersion() + 7
	if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version = %d", future)); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	_, err = store.Open(dir, clock.NewFake())
	var tooNew store.ErrSchemaTooNew
	if !errors.As(err, &tooNew) {
		t.Fatalf("opening a future database gave %v, want ErrSchemaTooNew", err)
	}
	if tooNew.Database != future || tooNew.Binary != store.SchemaVersion() {
		t.Errorf("ErrSchemaTooNew = %+v", tooNew)
	}
	for _, want := range []string{fmt.Sprint(future), fmt.Sprint(store.SchemaVersion())} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error %q does not name %s", err, want)
		}
	}
}

// --- transitions -------------------------------------------------------------

// Spec section 4.4: a backward move is refused and the stored state is left
// alone; delivery_state_raw is still overwritten with whatever Google last
// said. A forward skip is accepted.
func TestDeliveryTransitionsInTheStore(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	acct := seedAccount(t, st, "owner@example.com")
	if _, err := st.UpsertConversation(ctx, acct, gm.Conversation{
		SourceID: "c1", Folder: gm.FolderInbox, LastActivity: time.Unix(1, 0)}); err != nil {
		t.Fatal(err)
	}
	put := func(raw int32) store.UpsertMessageResult {
		t.Helper()
		state, ok := gm.DeliveryStateFor(raw)
		if !ok {
			t.Fatalf("status %d is unmapped", raw)
		}
		res, err := st.UpsertMessage(ctx, acct, "c1", gm.Message{
			SourceID: "m1", ConversationID: "c1", Text: "hello",
			Timestamp: time.Unix(1757000000, 0), StatusRaw: raw,
			Kind: gm.KindForStatus(raw), DeliveryState: state,
		})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	stateOf := func() (string, int32) {
		t.Helper()
		m, err := st.Message(ctx, store.MessageID(acct, "c1", "m1"))
		if err != nil {
			t.Fatal(err)
		}
		return m.DeliveryState, m.DeliveryStateRaw
	}

	put(5) // OUTGOING_SENDING
	if s, _ := stateOf(); s != string(gm.DeliveryStateSending) {
		t.Fatalf("state = %s, want sending", s)
	}
	put(2) // OUTGOING_DELIVERED -- a forward skip past `sent`
	if s, _ := stateOf(); s != string(gm.DeliveryStateDelivered) {
		t.Fatalf("a forward skip was refused: state = %s", s)
	}

	res := put(1) // OUTGOING_COMPLETE -- backwards
	if !res.TransitionRefused {
		t.Error("a backward move must be reported as refused")
	}
	s, raw := stateOf()
	if s != string(gm.DeliveryStateDelivered) {
		t.Errorf("a refused transition changed the stored state to %s", s)
	}
	if raw != 1 {
		t.Errorf("delivery_state_raw = %d, want the raw truth 1", raw)
	}
}

// A repeat whose content and raw status are both unchanged writes nothing at
// all, so a replay storm does not churn the WAL or bump updated_at_ms.
func TestUnchangedUpsertWritesNothing(t *testing.T) {
	st, clk := newStore(t)
	ctx := context.Background()
	acct := seedAccount(t, st, "owner@example.com")
	if _, err := st.UpsertConversation(ctx, acct, gm.Conversation{
		SourceID: "c1", Folder: gm.FolderInbox, LastActivity: time.Unix(1, 0)}); err != nil {
		t.Fatal(err)
	}
	m := gm.Message{SourceID: "m1", ConversationID: "c1", Text: "hello",
		Timestamp: time.Unix(1757000000, 0), StatusRaw: 100,
		Kind: gm.MessageKindMessage, DeliveryState: gm.DeliveryStateReceived}

	first, err := st.UpsertMessage(ctx, acct, "c1", m)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Inserted {
		t.Fatal("the first upsert must insert")
	}
	before, err := st.Message(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	clk.Advance(time.Hour)
	second, err := st.UpsertMessage(ctx, acct, "c1", m)
	if err != nil {
		t.Fatal(err)
	}
	if second.Inserted || second.Updated {
		t.Error("an identical replay must write nothing")
	}
	after, err := st.Message(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.UpdatedAtMS != before.UpdatedAtMS {
		t.Errorf("updated_at_ms moved on an identical replay: %d -> %d", before.UpdatedAtMS, after.UpdatedAtMS)
	}
}

// last_activity_ms is monotonic per conversation: a replayed old message
// cannot make a conversation jump to the top of the list.
func TestConversationActivityNeverMovesBackwards(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	acct := seedAccount(t, st, "owner@example.com")
	newer := time.Unix(1757000000, 0)
	older := newer.Add(-24 * time.Hour)

	id, err := st.UpsertConversation(ctx, acct, gm.Conversation{
		SourceID: "c1", Folder: gm.FolderInbox, LastActivity: newer})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertConversation(ctx, acct, gm.Conversation{
		SourceID: "c1", Folder: gm.FolderInbox, LastActivity: older}); err != nil {
		t.Fatal(err)
	}
	c, err := st.Conversation(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if c.LastActivityMS != newer.UnixMilli() {
		t.Errorf("last_activity_ms = %d, want %d", c.LastActivityMS, newer.UnixMilli())
	}
	if err := st.BumpConversationActivity(ctx, id, "msg_old", older.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	c, err = st.Conversation(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if c.LastActivityMS != newer.UnixMilli() {
		t.Errorf("a bump with an older timestamp moved last_activity_ms to %d", c.LastActivityMS)
	}
}

// System events are stored with kind='system' and excluded from listings
// unless include_system is set.
func TestSystemEventsAreExcludedByDefault(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	acct := seedAccount(t, st, "owner@example.com")
	if _, err := st.UpsertConversation(ctx, acct, gm.Conversation{
		SourceID: "c1", Folder: gm.FolderInbox, LastActivity: time.Unix(1, 0)}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		id  string
		raw int32
	}{{"m1", 100}, {"m2", 217}} { // 217 = TOMBSTONE_GROUP_RENAMED_GLOBAL
		state, _ := gm.DeliveryStateFor(tc.raw)
		if _, err := st.UpsertMessage(ctx, acct, "c1", gm.Message{
			SourceID: tc.id, ConversationID: "c1", Timestamp: time.Unix(1757000000, 0),
			StatusRaw: tc.raw, Kind: gm.KindForStatus(tc.raw), DeliveryState: state,
		}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := st.Messages(ctx, store.MessageFilter{AccountID: acct})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].SourceID != "m1" {
		t.Errorf("default listing returned %d rows, want only the message", len(got))
	}
	got, err = st.Messages(ctx, store.MessageFilter{AccountID: acct, IncludeSystem: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("include_system returned %d rows, want 2", len(got))
	}
	for _, m := range got {
		if m.SourceID == "m2" && m.Kind != string(gm.MessageKindSystem) {
			t.Errorf("a 200-279 status stored as kind=%q", m.Kind)
		}
	}
}

// Every query carries its account, or says explicitly that it does not.
func TestMessagesRefusesAQueryWithNoAccountPredicate(t *testing.T) {
	st, _ := newStore(t)
	if _, err := st.Messages(context.Background(), store.MessageFilter{}); err == nil {
		t.Fatal("a listing with no account predicate and no all-accounts marker must be refused")
	}
	if _, err := st.Messages(context.Background(), store.MessageFilter{AllAccounts: true}); err != nil {
		t.Fatalf("the explicit all-accounts form must be allowed: %v", err)
	}
}

// --- the data key and the session envelope -----------------------------------

func TestParseDataKey(t *testing.T) {
	hexKey := strings.Repeat("ab", 32)
	k, err := store.ParseDataKey(hexKey)
	if err != nil {
		t.Fatalf("hex: %v", err)
	}
	b64 := "q6urq6urq6urq6urq6urq6urq6urq6urq6urq6urq6s=" // the same 32 bytes
	k2, err := store.ParseDataKey(b64)
	if err != nil {
		t.Fatalf("base64: %v", err)
	}
	if k != k2 {
		t.Error("the two documented encodings of one key must parse to the same value")
	}
	for _, bad := range []string{"", "short", strings.Repeat("z", 64)} {
		if _, err := store.ParseDataKey(bad); !errors.Is(err, store.ErrBadDataKey) {
			t.Errorf("ParseDataKey(%q) = %v, want ErrBadDataKey", bad, err)
		}
	}
	// The four purposes derive four distinct subkeys.
	seen := map[string]bool{}
	for _, info := range []string{store.InfoSession, store.InfoAttachmentKey, store.InfoTicket, store.InfoCursor} {
		sub, err := k.Derive(info)
		if err != nil {
			t.Fatal(err)
		}
		if seen[string(sub)] {
			t.Errorf("%s derives a subkey another purpose already uses", info)
		}
		seen[string(sub)] = true
	}
}

func TestSessionEnvelope(t *testing.T) {
	dir := t.TempDir()
	key, err := store.ParseDataKey(strings.Repeat("11", 32))
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := store.NewSessionStore(dir, key)
	if err != nil {
		t.Fatal(err)
	}
	acctA := store.AccountID("a@example.com")
	acctB := store.AccountID("b@example.com")
	payload := []byte(`{"cookies":{"SID":"fixture"}}`)

	if err := sessions.Save(acctA, payload); err != nil {
		t.Fatal(err)
	}
	got, err := sessions.Load(acctA)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Errorf("round trip returned %q", got)
	}

	// Mode 0600 in a 0700 directory.
	fi, err := os.Stat(sessions.Path(acctA))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("session file mode is %o, want 600", fi.Mode().Perm())
	}
	di, err := os.Stat(sessions.Dir())
	if err != nil {
		t.Fatal(err)
	}
	if di.Mode().Perm() != 0o700 {
		t.Errorf("sessions/ mode is %o, want 700", di.Mode().Perm())
	}

	// The account ID is in the AEAD's associated data, so a session file
	// renamed to another account's name fails to open rather than silently
	// loading the wrong account.
	raw, err := os.ReadFile(sessions.Path(acctA))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sessions.Path(acctB), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := sessions.Load(acctB); !errors.Is(err, store.ErrSessionUndecryptable) {
		t.Fatalf("a renamed session opened as %v, want ErrSessionUndecryptable", err)
	}

	// A different data key cannot decrypt it, and says exactly that.
	otherKey, err := store.ParseDataKey(strings.Repeat("22", 32))
	if err != nil {
		t.Fatal(err)
	}
	other, err := store.NewSessionStore(dir, otherKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.Load(acctA); !errors.Is(err, store.ErrSessionUndecryptable) {
		t.Fatalf("the wrong key opened the session: %v", err)
	}

	// No plaintext on disk.
	if strings.Contains(string(raw), "fixture") {
		t.Error("the session file contains plaintext")
	}
	if !strings.HasPrefix(string(raw), "AGMS1") {
		t.Errorf("the envelope does not start with the AGMS1 magic")
	}

	// Shredding removes the file and nothing else.
	if err := sessions.Shred(acctA); err != nil {
		t.Fatal(err)
	}
	if sessions.Present(acctA) {
		t.Error("the session file survived a shred")
	}
	if _, err := sessions.Load(acctA); !errors.Is(err, store.ErrNoSession) {
		t.Errorf("loading a shredded session gave %v, want ErrNoSession", err)
	}
	// Shredding twice is not an error.
	if err := sessions.Shred(acctA); err != nil {
		t.Errorf("shredding twice: %v", err)
	}
}

// --- account lifecycle -------------------------------------------------------

func TestAccountStatesAndAmbiguityRule(t *testing.T) {
	// pairing accounts do not count toward the section 7.3 ambiguity rule:
	// an abandoned pair can never start demanding account_id on writes for
	// an account that may never exist.
	if store.CountsTowardAmbiguity(store.StatePairing) {
		t.Error("pairing must not count toward the ambiguity rule")
	}
	for _, s := range []store.AccountState{
		store.StateConnected, store.StateDegraded, store.StateParked,
		store.StateError, store.StateSignedOut, store.StateAccountChanged,
	} {
		if !store.CountsTowardAmbiguity(s) {
			t.Errorf("%s must count toward the ambiguity rule", s)
		}
	}
}

func TestPairingRowsAreSweptAndAreNeverARestingState(t *testing.T) {
	st, clk := newStore(t)
	ctx := context.Background()
	id := store.AccountID("owner@example.com")
	if err := st.UpsertAccount(ctx, store.Account{
		ID: id, GoogleAccount: "owner@example.com", State: store.StatePairing,
	}); err != nil {
		t.Fatal(err)
	}
	// A live pairing inside the timeout survives.
	deleted, err := st.SweepAbandonedPairings(ctx, map[string]bool{id: true}, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(deleted) != 0 {
		t.Fatalf("a live pairing was swept: %v", deleted)
	}
	// One with no live pairing behind it does not.
	deleted, err = st.SweepAbandonedPairings(ctx, nil, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(deleted) != 1 || deleted[0] != id {
		t.Fatalf("sweep deleted %v, want [%s]", deleted, id)
	}
	if _, err := st.Account(ctx, id); !errors.Is(err, store.ErrAccountNotFound) {
		t.Errorf("the pairing row survived: %v", err)
	}

	// A row that had been connected is never deleted by the sweep.
	if err := st.UpsertAccount(ctx, store.Account{
		ID: id, GoogleAccount: "owner@example.com", State: store.StateConnected,
		PairedAtMS: clk.Now().UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SweepAbandonedPairings(ctx, nil, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Account(ctx, id); err != nil {
		t.Errorf("the sweep deleted a connected account: %v", err)
	}
}

func TestSignOutStateAndSessionPresence(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	acct := seedAccount(t, st, "owner@example.com")
	if err := st.SetSessionPresent(ctx, acct, true); err != nil {
		t.Fatal(err)
	}
	if err := st.SetAccountState(ctx, acct, store.StateSignedOut, store.ReasonCookiesExpired); err != nil {
		t.Fatal(err)
	}
	a, err := st.Account(ctx, acct)
	if err != nil {
		t.Fatal(err)
	}
	if a.State != store.StateSignedOut || a.StateReason != store.ReasonCookiesExpired {
		t.Errorf("state = %s/%s", a.State, a.StateReason)
	}
	if err := st.SetAccountState(ctx, "acct_"+uuid.NewString(), store.StateError, ""); !errors.Is(err, store.ErrAccountNotFound) {
		t.Errorf("setting the state of an unknown account gave %v", err)
	}
}

// F-3, plant R-M6: the SQL trigger is the backstop for a writer that does not
// go through UpsertMessage -- a repair script, a later backfill, a bare
// sqlite3 session. Every store test until now went through UpsertMessage,
// which applies the Go rule and never lets an illegal value reach SQLite, so
// the SQL half of "enforced by a SQL trigger as well as in Go" was dead
// weight.
//
// This walks every ordered pair of delivery states with RAW SQL and fails if
// the trigger and gm.TransitionAllowed disagree on even one, so the two
// cannot drift apart.
func TestTriggerAgreesWithTransitionAllowed(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	acct := seedAccount(t, st, "owner@example.com")
	convID, err := st.UpsertConversation(ctx, acct, gm.Conversation{
		SourceID: "c1", Folder: gm.FolderInbox, LastActivity: time.Unix(1, 0)})
	if err != nil {
		t.Fatal(err)
	}

	states := []gm.DeliveryState{
		gm.DeliveryStateSending, gm.DeliveryStateSent, gm.DeliveryStateDelivered,
		gm.DeliveryStateRead, gm.DeliveryStateFailed, gm.DeliveryStateCanceled,
		gm.DeliveryStateDeleted, gm.DeliveryStateReceived, gm.DeliveryStateDownloading,
		gm.DeliveryStateDownloadFailed, gm.DeliveryStateUnknown,
	}

	// rawWrite bypasses UpsertMessage entirely: this is the writer the
	// trigger exists for.
	rawWrite := func(id, direction string, state gm.DeliveryState) error {
		return st.Write(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `
				INSERT INTO messages (id, account_id, conversation_id, source_id, kind,
				    direction, delivery_state, delivery_state_raw, is_deleted,
				    sent_at_ms, ingested_at_ms, updated_at_ms, content_hash)
				VALUES (?,?,?,?,'message',?,?,0,0,1,1,1,'hash')`,
				id, acct, convID, id, direction, string(state))
			return err
		})
	}
	rawMove := func(id string, to gm.DeliveryState) error {
		return st.Write(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx,
				`UPDATE messages SET delivery_state = ? WHERE id = ?`, string(to), id)
			return err
		})
	}

	n := 0
	for _, from := range states {
		for _, to := range states {
			n++
			id := "msg_raw-" + string(from) + "-" + string(to)
			if err := rawWrite(id, "outgoing", from); err != nil {
				t.Fatalf("seeding %s: %v", id, err)
			}
			err := rawMove(id, to)
			refusedBySQL := err != nil
			allowedByGo := gm.TransitionAllowed(from, to)
			if refusedBySQL == allowedByGo {
				t.Errorf("%s -> %s: the trigger %s but gm.TransitionAllowed says %v",
					from, to,
					map[bool]string{true: "REFUSED", false: "ALLOWED"}[refusedBySQL],
					allowedByGo)
			}
			if refusedBySQL && !strings.Contains(err.Error(), "delivery_state may not move backwards") {
				t.Errorf("%s -> %s refused with an unhelpful message: %v", from, to, err)
			}
		}
	}
	if n != len(states)*len(states) {
		t.Fatalf("walked %d pairs, want %d", n, len(states)*len(states))
	}

	// The four moves the previous rank-based trigger let through, named so a
	// regression is legible rather than buried in the walk above.
	// Plant R-M6, 2026-09-06.
	for _, tc := range [][2]gm.DeliveryState{
		{gm.DeliveryStateFailed, gm.DeliveryStateSent},
		{gm.DeliveryStateDeleted, gm.DeliveryStateSending},
		{gm.DeliveryStateDelivered, gm.DeliveryStateCanceled},
		{gm.DeliveryStateCanceled, gm.DeliveryStateDelivered},
	} {
		id := "msg_named-" + string(tc[0]) + "-" + string(tc[1])
		if err := rawWrite(id, "outgoing", tc[0]); err != nil {
			t.Fatal(err)
		}
		if err := rawMove(id, tc[1]); err == nil {
			t.Errorf("the trigger allowed %s -> %s; a failed message must not come back as sent", tc[0], tc[1])
		}
	}

	// The trigger is scoped to outgoing messages: an incoming one is Google's
	// own report and is not on the ladder.
	if err := rawWrite("msg_incoming", "incoming", gm.DeliveryStateReceived); err != nil {
		t.Fatal(err)
	}
	if err := rawMove("msg_incoming", gm.DeliveryStateDownloading); err != nil {
		t.Errorf("the trigger fired on an incoming message: %v", err)
	}
}
