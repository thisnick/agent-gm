package store_test

// The listing and search routes of spec section 7.6, in both forms, and the
// two-accounts-no-cross-talk assertions of section 13.2.

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/thisnick/agent-gm/internal/gm"
	"github.com/thisnick/agent-gm/internal/store"
)

// A fictional 555 number. No real phone number ever appears in this repo.
const (
	fixturePhoneA = "+12025550101"
	fixturePhoneB = "+12025550102"
	fixtureMine   = "+12025550100"
)

type fixture struct {
	accountID string
	convID    string
	messages  []string
}

// seedThread writes one conversation with two participants -- the account's
// own is_me participant and one peer -- and the given message texts, one
// millisecond apart, newest last.
func seedThread(t *testing.T, st *store.Store, accountID, convSource, name, peerPhone string, group bool, folder gm.Folder, texts ...string) fixture {
	t.Helper()
	ctx := context.Background()
	convID, err := st.UpsertConversation(ctx, accountID, gm.Conversation{
		SourceID: convSource, Name: name, IsGroup: group,
		Type: gm.ConversationTypeRCS, SendModeRaw: gm.SendModeAuto, Folder: folder,
		LastActivity: time.UnixMilli(1700000000000 + int64(len(texts))),
		Participants: []gm.Participant{
			{SourceID: convSource + "-me", DisplayName: "Me", PhoneE164: fixtureMine, IsMe: true, IsVisible: true},
			{SourceID: convSource + "-peer", DisplayName: name, PhoneE164: peerPhone, IsVisible: true},
		},
	})
	if err != nil {
		t.Fatalf("seeding conversation %s: %v", convSource, err)
	}
	f := fixture{accountID: accountID, convID: convID}
	// GOOGLE's participant IDs, which is what a real message carries. The
	// store derives the part_ ID; a fixture that handed it the derived value
	// would be pre-compensating for the bug this file exists to catch, and
	// that is exactly what it used to do -- which is why `sender=me`
	// returning an empty page in production passed here for a whole slice.
	me := convSource + "-me"
	peer := convSource + "-peer"
	for i, text := range texts {
		sender, raw, state, dir := peer, int32(100), gm.DeliveryStateReceived, gm.DirectionIncoming
		if i%2 == 1 {
			sender, raw, state, dir = me, int32(1), gm.DeliveryStateSent, gm.DirectionOutgoing
		}
		_ = dir
		res, err := st.UpsertMessage(ctx, accountID, convSource, gm.Message{
			SourceID: fmt.Sprintf("%s-msg-%d", convSource, i), ParticipantID: sender,
			Text: text, Timestamp: time.UnixMilli(1700000000000 + int64(i)),
			StatusRaw: raw, Kind: gm.MessageKindMessage, DeliveryState: state,
		})
		if err != nil {
			t.Fatalf("seeding message %d: %v", i, err)
		}
		f.messages = append(f.messages, res.ID)
	}
	return f
}

func ids(ms []store.Message) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.ID
	}
	return out
}

// --- listing -----------------------------------------------------------------

func TestConversationListingFiltersInBothForms(t *testing.T) {
	ctx := context.Background()
	st, _ := newStore(t)
	one := seedAccount(t, st, "one@example.com")
	two := seedAccount(t, st, "two@example.com")

	seedThread(t, st, one, "c-one-a", "Alex", fixturePhoneA, false, gm.FolderInbox, "hello")
	seedThread(t, st, one, "c-one-b", "Book club", fixturePhoneB, true, gm.FolderArchive, "hi")
	seedThread(t, st, two, "c-two-a", "Alex", fixturePhoneA, false, gm.FolderInbox, "hey")

	all, err := st.ListConversations(ctx, store.ConversationQuery{AllAccounts: true})
	if err != nil {
		t.Fatalf("listing all: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("the all-accounts default returned %d conversations, want 3", len(all))
	}

	scoped, err := st.ListConversations(ctx, store.ConversationQuery{AccountID: one})
	if err != nil {
		t.Fatalf("listing one account: %v", err)
	}
	if len(scoped) != 2 {
		t.Fatalf("account one has %d conversations, want 2", len(scoped))
	}
	for _, c := range scoped {
		if c.AccountID != one {
			t.Errorf("a %s row leaked into account %s's listing", c.AccountID, one)
		}
	}

	cases := []struct {
		name string
		q    store.ConversationQuery
		want int
	}{
		{"folder", store.ConversationQuery{AllAccounts: true, Folder: "archived"}, 1},
		{"group_only", store.ConversationQuery{AllAccounts: true, GroupOnly: new(true)}, 1},
		{"type", store.ConversationQuery{AllAccounts: true, Type: "rcs"}, 3},
		{"query", store.ConversationQuery{AllAccounts: true, Query: "alex"}, 2},
		{"query is case-insensitive", store.ConversationQuery{AllAccounts: true, Query: "ALEX"}, 2},
		{"participant, all accounts", store.ConversationQuery{AllAccounts: true, ParticipantPhone: fixturePhoneA}, 2},
		{"participant, one account", store.ConversationQuery{AccountID: one, ParticipantPhone: fixturePhoneA}, 1},
		{"unread_only", store.ConversationQuery{AllAccounts: true, UnreadOnly: true}, 0},
	}
	for _, c := range cases {
		got, err := st.ListConversations(ctx, c.q)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if len(got) != c.want {
			t.Errorf("%s: %d conversations, want %d", c.name, len(got), c.want)
		}
	}

	if _, err := st.ListConversations(ctx, store.ConversationQuery{}); !errors.Is(err, store.ErrNoAccountPredicate) {
		t.Errorf("an unstated account scope was accepted: %v", err)
	}
}

func TestMessageListingFiltersAndOrdering(t *testing.T) {
	ctx := context.Background()
	st, _ := newStore(t)
	one := seedAccount(t, st, "one@example.com")
	two := seedAccount(t, st, "two@example.com")

	a := seedThread(t, st, one, "c-one-a", "Alex", fixturePhoneA, false, gm.FolderInbox,
		"first", "second", "third", "fourth")
	seedThread(t, st, two, "c-two-a", "Alex", fixturePhoneA, false, gm.FolderInbox, "elsewhere")

	got, err := st.ListMessages(ctx, store.MessageQuery{ConversationID: a.convID})
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("listed %d messages, want 4", len(got))
	}
	// Newest first, never ingestion order.
	for i := 1; i < len(got); i++ {
		if got[i-1].SentAtMS < got[i].SentAtMS {
			t.Errorf("messages are not newest-first at %d", i)
		}
	}
	if got[0].Text != "fourth" {
		t.Errorf("the newest message is %q, want %q", got[0].Text, "fourth")
	}

	outgoing, err := st.ListMessages(ctx, store.MessageQuery{ConversationID: a.convID, Direction: "outgoing"})
	if err != nil {
		t.Fatalf("direction filter: %v", err)
	}
	if len(outgoing) != 2 {
		t.Errorf("direction=outgoing gave %d, want 2", len(outgoing))
	}

	mine, err := st.ListMessages(ctx, store.MessageQuery{AllAccounts: true, SenderMe: true})
	if err != nil {
		t.Fatalf("sender=me: %v", err)
	}
	// sender=me across accounts is EVERY account's own participant.
	if len(mine) != 2 {
		t.Errorf("sender=me across accounts gave %d, want 2", len(mine))
	}

	byPhone, err := st.ListMessages(ctx, store.MessageQuery{AllAccounts: true, SenderPhone: fixturePhoneA})
	if err != nil {
		t.Fatalf("sender by phone: %v", err)
	}
	if len(byPhone) != 3 {
		t.Errorf("sender=<peer> across accounts gave %d, want 3", len(byPhone))
	}

	window, err := st.ListMessages(ctx, store.MessageQuery{
		ConversationID: a.convID, AfterMS: 1700000000001, BeforeMS: 1700000000002})
	if err != nil {
		t.Fatalf("window: %v", err)
	}
	if len(window) != 2 {
		t.Errorf("the after/before window gave %d, want 2", len(window))
	}

	state, err := st.ListMessages(ctx, store.MessageQuery{ConversationID: a.convID, DeliveryState: "sent"})
	if err != nil {
		t.Fatalf("delivery_state: %v", err)
	}
	if len(state) != 2 {
		t.Errorf("delivery_state=sent gave %d, want 2", len(state))
	}

	if _, err := st.ListMessages(ctx, store.MessageQuery{}); !errors.Is(err, store.ErrNoAccountPredicate) {
		t.Errorf("an unstated account scope was accepted: %v", err)
	}
}

func TestHasAttachmentFilter(t *testing.T) {
	ctx := context.Background()
	st, _ := newStore(t)
	acct := seedAccount(t, st, "one@example.com")
	f := seedThread(t, st, acct, "c-a", "Alex", fixturePhoneA, false, gm.FolderInbox, "one", "two")

	if _, err := st.UpsertAttachment(ctx, acct, f.messages[0], gm.Attachment{
		PartIndex: 0, MediaID: "media-1", Filename: "IMG_0421.jpg",
		MimeType: "image/jpeg", SizeBytes: 184320,
	}, nil, store.DownloadStateAvailable); err != nil {
		t.Fatalf("attaching: %v", err)
	}

	with := true
	got, err := st.ListMessages(ctx, store.MessageQuery{ConversationID: f.convID, HasAttachment: &with})
	if err != nil {
		t.Fatalf("has_attachment: %v", err)
	}
	if len(got) != 1 || got[0].ID != f.messages[0] {
		t.Errorf("has_attachment=true gave %v", ids(got))
	}
	without := false
	got, err = st.ListMessages(ctx, store.MessageQuery{ConversationID: f.convID, HasAttachment: &without})
	if err != nil {
		t.Fatalf("has_attachment=false: %v", err)
	}
	if len(got) != 1 || got[0].ID != f.messages[1] {
		t.Errorf("has_attachment=false gave %v", ids(got))
	}

	list, err := st.AttachmentsForMessage(ctx, f.messages[0])
	if err != nil {
		t.Fatalf("reading attachments: %v", err)
	}
	if len(list) != 1 || list[0].AccountID != acct {
		t.Fatalf("attachment rows: %+v", list)
	}
	one, err := st.Attachment(ctx, list[0].ID)
	if err != nil {
		t.Fatalf("reading one attachment: %v", err)
	}
	if one.MimeType != "image/jpeg" || one.SizeBytes != 184320 {
		t.Errorf("attachment round trip lost data: %+v", one)
	}
	if _, err := st.Attachment(ctx, "att_00000000-0000-7000-8000-000000000000"); !errors.Is(err, store.ErrAttachmentNotFound) {
		t.Errorf("a missing attachment gave %v", err)
	}
}

func TestMessageContext(t *testing.T) {
	ctx := context.Background()
	st, _ := newStore(t)
	acct := seedAccount(t, st, "one@example.com")
	f := seedThread(t, st, acct, "c-a", "Alex", fixturePhoneA, false, gm.FolderInbox,
		"m0", "m1", "m2", "m3", "m4", "m5", "m6")

	got, err := st.MessageContextAround(ctx, f.messages[3], 2, 2, false)
	if err != nil {
		t.Fatalf("context: %v", err)
	}
	if got.Message.ID != f.messages[3] {
		t.Errorf("the anchor is %s", got.Message.ID)
	}
	if len(got.Before) != 2 || got.Before[0].Text != "m2" || got.Before[1].Text != "m1" {
		t.Errorf("before = %v", texts(got.Before))
	}
	if len(got.After) != 2 || got.After[0].Text != "m4" || got.After[1].Text != "m5" {
		t.Errorf("after = %v", texts(got.After))
	}

	// The defaults are 5 and the cap is 100.
	def, err := st.MessageContextAround(ctx, f.messages[3], 0, 0, false)
	if err != nil {
		t.Fatalf("default context: %v", err)
	}
	if len(def.Before) != 3 || len(def.After) != 3 {
		t.Errorf("defaults gave before=%d after=%d", len(def.Before), len(def.After))
	}
}

func texts(ms []store.Message) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.Text
	}
	return out
}

// --- search ------------------------------------------------------------------

func TestSearchModesAndInjection(t *testing.T) {
	ctx := context.Background()
	st, _ := newStore(t)
	one := seedAccount(t, st, "one@example.com")
	two := seedAccount(t, st, "two@example.com")

	seedThread(t, st, one, "c-one", "Alex", fixturePhoneA, false, gm.FolderInbox,
		"marmalade sandwich", "sandwich marmalade", "just marmalade")
	seedThread(t, st, two, "c-two", "Alex", fixturePhoneA, false, gm.FolderInbox,
		"marmalade elsewhere")

	all, err := st.SearchMessages(ctx, store.SearchQuery{Q: "marmalade", AllAccounts: true})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(all) != 4 {
		t.Fatalf("the all-accounts search found %d, want 4", len(all))
	}
	for i := 1; i < len(all); i++ {
		if all[i-1].Message.SentAtMS < all[i].Message.SentAtMS {
			t.Errorf("search results are not newest-first at %d", i)
		}
	}

	scoped, err := st.SearchMessages(ctx, store.SearchQuery{Q: "marmalade", AccountID: two})
	if err != nil {
		t.Fatalf("scoped search: %v", err)
	}
	if len(scoped) != 1 || scoped[0].Message.AccountID != two {
		t.Errorf("the scoped search returned %d rows from the wrong accounts", len(scoped))
	}

	// `words` ANDs the terms, so order does not matter.
	words, err := st.SearchMessages(ctx, store.SearchQuery{
		Q: "sandwich marmalade", Mode: store.SearchModeWords, AllAccounts: true})
	if err != nil {
		t.Fatalf("words search: %v", err)
	}
	if len(words) != 2 {
		t.Errorf("words mode found %d, want both orderings", len(words))
	}

	// `exact` matches the phrase as written.
	exact, err := st.SearchMessages(ctx, store.SearchQuery{
		Q: "marmalade sandwich", Mode: store.SearchModeExact, AllAccounts: true})
	if err != nil {
		t.Fatalf("exact search: %v", err)
	}
	if len(exact) != 1 || exact[0].Message.Text != "marmalade sandwich" {
		t.Errorf("exact mode found %v", exact)
	}

	if exact[0].Snippet == "" {
		t.Error("no snippet was produced")
	}
}

// There is no way to inject FTS5 operator syntax from the caller's q, and no
// mode named after a SQLite extension (spec section 7.6).
func TestSearchCannotInjectFTS5Syntax(t *testing.T) {
	ctx := context.Background()
	st, _ := newStore(t)
	acct := seedAccount(t, st, "one@example.com")
	seedThread(t, st, acct, "c-a", "Alex", fixturePhoneA, false, gm.FolderInbox,
		"marmalade", "sandwich")

	hostile := []string{
		`marmalade OR sandwich`,
		`marmalade NEAR sandwich`,
		`"marmalade" OR "sandwich"`,
		`marmalade*`,
		`text : marmalade`,
		`(marmalade`,
		`marmalade" OR "sandwich`,
		`^marmalade`,
		`marmalade AND (sandwich OR marmalade)`,
	}
	for _, q := range hostile {
		got, err := st.SearchMessages(ctx, store.SearchQuery{Q: q, AccountID: acct})
		if err != nil {
			t.Fatalf("%q: search errored, so the operator syntax reached FTS5: %v", q, err)
		}
		// Every term is required, so a query naming both words matches
		// neither message; none of these may behave as an OR.
		if len(got) > 1 {
			t.Errorf("%q matched %d messages; operator syntax appears to have been honoured", q, len(got))
		}
	}

	// The tokeniser itself: operators become ordinary words.
	if q := store.FTSQuery(`marmalade OR sandwich`); q != `"marmalade" AND "OR" AND "sandwich"` {
		t.Errorf("FTSQuery = %s", q)
	}
	if q := store.FTSQuery(`"quoted" AND (paren)`); !strings.Contains(q, `"quoted"`) || strings.Contains(q, "(") {
		t.Errorf("FTSQuery left punctuation in: %s", q)
	}
	if q := store.FTSQuery("   "); q != "" {
		t.Errorf("an all-punctuation query produced %q", q)
	}
}

// --- contacts and reactions --------------------------------------------------

func TestContactsListingAndParticipantLinking(t *testing.T) {
	ctx := context.Background()
	st, _ := newStore(t)
	one := seedAccount(t, st, "one@example.com")
	two := seedAccount(t, st, "two@example.com")

	// The same person in two accounts is two contact rows.
	var contactIDs []string
	for _, acct := range []string{one, two} {
		id, err := st.UpsertContact(ctx, acct, gm.Contact{
			SourceID: "c-one-peer", DisplayName: "Alex", PhoneE164: fixturePhoneA, IsTop: true,
		}, "sha256-of-avatar")
		if err != nil {
			t.Fatalf("upserting contact: %v", err)
		}
		contactIDs = append(contactIDs, id)
	}
	if contactIDs[0] == contactIDs[1] {
		t.Fatal("the same person in two accounts produced one contact row")
	}

	seedThread(t, st, one, "c-one", "Alex", fixturePhoneA, false, gm.FolderInbox, "hi")

	all, err := st.ListContacts(ctx, store.ContactQuery{AllAccounts: true})
	if err != nil {
		t.Fatalf("listing contacts: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("listed %d contacts, want 2", len(all))
	}
	for _, c := range all {
		if c.AccountID == "" {
			t.Error("a contact row is unattributable: no account_id")
		}
	}
	scoped, err := st.ListContacts(ctx, store.ContactQuery{AccountID: one, Query: "ale", Top: true})
	if err != nil {
		t.Fatalf("filtered contacts: %v", err)
	}
	if len(scoped) != 1 || scoped[0].AccountID != one {
		t.Errorf("the filtered contact list gave %d rows", len(scoped))
	}
	if _, err := st.ListContacts(ctx, store.ContactQuery{}); !errors.Is(err, store.ErrNoAccountPredicate) {
		t.Errorf("an unstated account scope was accepted: %v", err)
	}

	// participants.contact_id holds the derived contact_ ID of this account's
	// contact, never Google's own, and never another account's.
	if err := st.RelinkAccountContacts(ctx, one); err != nil {
		t.Fatalf("relinking: %v", err)
	}
	ps, err := st.Participants(ctx, store.ConversationID(one, "c-one"))
	if err != nil {
		t.Fatalf("reading participants: %v", err)
	}
	var linked int
	for _, p := range ps {
		if p.ContactID != "" {
			linked++
			if p.ContactID != store.ContactID(one, p.SourceID) {
				t.Errorf("participant %s links to %s", p.SourceID, p.ContactID)
			}
		}
	}
	if linked != 1 {
		t.Errorf("%d participants were linked to a contact, want 1", linked)
	}
}

// Reaction upsert is a SET-REPLACE of a message's whole reaction set, one row
// per (message, participant) -- Google's picker is single-select (D23).
func TestReactionsAreASetReplaceOneRowPerParticipant(t *testing.T) {
	ctx := context.Background()
	st, _ := newStore(t)
	acct := seedAccount(t, st, "one@example.com")
	f := seedThread(t, st, acct, "c-a", "Alex", fixturePhoneA, false, gm.FolderInbox, "hi")
	msg := f.messages[0]
	me := store.ParticipantID(f.convID, "c-a-me")
	peer := store.ParticipantID(f.convID, "c-a-peer")

	thumb := "\U0001F44D"
	heart := "❤️"
	if err := st.ReplaceReactions(ctx, "", msg, me, []gm.Reaction{
		{Emoji: &thumb, Type: gm.EmojiTypeLike, ParticipantIDs: []string{me, peer}},
	}); err != nil {
		t.Fatalf("first reaction set: %v", err)
	}
	got, err := st.ReactionsForMessage(ctx, msg)
	if err != nil {
		t.Fatalf("reading reactions: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("%d reaction rows, want one per participant", len(got))
	}
	var mineSeen bool
	for _, r := range got {
		if r.ParticipantID == me {
			mineSeen = r.IsMine
		}
	}
	if !mineSeen {
		t.Error("this account's own reaction is not marked is_mine")
	}

	// A second, different emoji from the same person REPLACES the first: one
	// row, not two.
	if err := st.ReplaceReactions(ctx, "", msg, me, []gm.Reaction{
		{Emoji: &heart, Type: gm.EmojiTypeLove, ParticipantIDs: []string{me}},
	}); err != nil {
		t.Fatalf("second reaction set: %v", err)
	}
	got, err = st.ReactionsForMessage(ctx, msg)
	if err != nil {
		t.Fatalf("reading reactions: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("%d reaction rows after the replace, want 1 -- the peer's removal was not applied", len(got))
	}
	if got[0].Emoji != heart || got[0].EmojiType != string(gm.EmojiTypeLove) {
		t.Errorf("the reaction was not switched: %+v", got[0])
	}

	// A type with no unicode of its own serves {"emoji": null, "type": ...}
	// rather than being dropped (spec section 3.7).
	if err := st.ReplaceReactions(ctx, "", msg, me, []gm.Reaction{
		{Emoji: nil, Type: gm.EmojiTypeEmotify, ParticipantIDs: []string{peer}},
	}); err != nil {
		t.Fatalf("emotify: %v", err)
	}
	got, err = st.ReactionsForMessage(ctx, msg)
	if err != nil {
		t.Fatalf("reading reactions: %v", err)
	}
	if len(got) != 1 || got[0].HasEmoji || got[0].EmojiType != string(gm.EmojiTypeEmotify) {
		t.Errorf("an emoji-less reaction was dropped or mangled: %+v", got)
	}
}

// --- two accounts, no cross-talk ---------------------------------------------

func TestTwoAccountsDoNotCrossTalk(t *testing.T) {
	ctx := context.Background()
	st, _ := newStore(t)
	one := seedAccount(t, st, "one@example.com")
	two := seedAccount(t, st, "two@example.com")

	// The same Google conversation ID in both accounts is two threads on two
	// phones, with two conv_ IDs (spec section 5.4).
	a := seedThread(t, st, one, "shared-source", "Alex", fixturePhoneA, false, gm.FolderInbox, "mine")
	b := seedThread(t, st, two, "shared-source", "Alex", fixturePhoneA, false, gm.FolderInbox, "theirs")
	if a.convID == b.convID {
		t.Fatal("one Google conversation ID in two accounts collapsed to one row")
	}

	for _, c := range []struct{ acct, conv string }{{one, a.convID}, {two, b.convID}} {
		conv, err := st.Conversation(ctx, c.conv)
		if err != nil {
			t.Fatalf("reading conversation: %v", err)
		}
		if conv.AccountID != c.acct {
			t.Errorf("conversation %s is under %s, want %s", c.conv, conv.AccountID, c.acct)
		}
	}

	msgsOne, err := st.ListMessages(ctx, store.MessageQuery{AccountID: one})
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	if len(msgsOne) != 1 || msgsOne[0].Text != "mine" {
		t.Errorf("account one sees %v", texts(msgsOne))
	}
}

// --- settings and audit ------------------------------------------------------

func TestSettingsPersistence(t *testing.T) {
	ctx := context.Background()
	st, _ := newStore(t)

	if _, err := st.Setting(ctx, "accounts.max_concurrent"); !errors.Is(err, store.ErrSettingNotFound) {
		t.Errorf("an unset key gave %v, want ErrSettingNotFound", err)
	}
	if err := st.SetSetting(ctx, "accounts.max_concurrent", "8"); err != nil {
		t.Fatalf("setting: %v", err)
	}
	// Written as arithmetic rather than as a bare literal: a bare
	// ten-digit number is NANP-shaped, and `devbox run no-real-numbers`
	// cannot tell a byte count from a phone number by looking at it. The
	// check erring that way is correct -- this repository is public
	// (spec section 13.3) -- so the value moves rather than the check.
	cacheMax := strconv.FormatInt(2*1024*1024*1024, 10)
	if err := st.SetSetting(ctx, "media.cache_max_bytes", cacheMax); err != nil {
		t.Fatalf("setting: %v", err)
	}
	got, err := st.Setting(ctx, "accounts.max_concurrent")
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	if got.ValueJSON != "8" || got.UpdatedAtMS == 0 {
		t.Errorf("setting round trip gave %+v", got)
	}
	if err := st.SetSetting(ctx, "accounts.max_concurrent", "4"); err != nil {
		t.Fatalf("overwriting: %v", err)
	}
	list, err := st.Settings(ctx)
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	if len(list) != 2 || list[0].Key != "accounts.max_concurrent" || list[0].ValueJSON != "4" {
		t.Errorf("settings list = %+v", list)
	}
	if err := st.DeleteSetting(ctx, "accounts.max_concurrent"); err != nil {
		t.Fatalf("deleting: %v", err)
	}
	if _, err := st.Setting(ctx, "accounts.max_concurrent"); !errors.Is(err, store.ErrSettingNotFound) {
		t.Errorf("the deleted key survived: %v", err)
	}
}

func TestAuditIsAppendOnlyAndFilterable(t *testing.T) {
	ctx := context.Background()
	st, clk := newStore(t)
	acct := seedAccount(t, st, "one@example.com")

	kinds := []string{"account.paired", "account.state_changed", "auth.admin_session_minted"}
	for _, kind := range kinds {
		account := acct
		if strings.HasPrefix(kind, "auth.") {
			account = "" // server-wide events carry no account
		}
		if _, err := st.AppendAudit(ctx, store.AuditEvent{
			Kind: kind, AccountID: account, AuthorizationID: "auth_1",
			Result: store.AuditOK, PayloadJSON: `{"counts":{"messages":3}}`,
		}); err != nil {
			t.Fatalf("appending %s: %v", kind, err)
		}
		clk.Advance(time.Second)
	}

	all, err := st.ListAuditEvents(ctx, store.AuditQuery{})
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("listed %d audit rows, want 3", len(all))
	}
	if all[0].Kind != "auth.admin_session_minted" {
		t.Errorf("audit rows are not newest-first: %s first", all[0].Kind)
	}

	cases := []struct {
		name string
		q    store.AuditQuery
		want int
	}{
		{"kind", store.AuditQuery{Kind: "account.paired"}, 1},
		{"kind_prefix", store.AuditQuery{KindPrefix: "account."}, 2},
		{"account_id", store.AuditQuery{AccountID: acct}, 2},
		{"authorization_id", store.AuditQuery{AuthorizationID: "auth_1"}, 3},
		{"prefix is not a wildcard", store.AuditQuery{KindPrefix: "acc%"}, 0},
	}
	for _, c := range cases {
		got, err := st.ListAuditEvents(ctx, c.q)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if len(got) != c.want {
			t.Errorf("%s: %d rows, want %d", c.name, len(got), c.want)
		}
	}
}

// The audit package offers no update and no delete, ever (spec section 12.4).
func TestTheStoreExposesNoWayToRewriteAnAuditRow(t *testing.T) {
	for _, name := range []string{"UpdateAudit", "DeleteAudit", "DeleteAuditEvents", "SetAudit"} {
		if hasStoreMethod(name) {
			t.Errorf("store.Store has a %s method; audit rows are never rewritten", name)
		}
	}
}

func hasStoreMethod(name string) bool {
	_, ok := reflect.TypeOf(&store.Store{}).MethodByName(name)
	return ok
}

func TestIDPrefixesForUploadsAndAudit(t *testing.T) {
	up := store.UploadID()
	if !strings.HasPrefix(up, store.PrefixUpload) || !store.HasPrefix(up, store.PrefixUpload) {
		t.Errorf("UploadID = %s, want a %s-prefixed UUID", up, store.PrefixUpload)
	}
	if store.UploadID() == up {
		t.Error("UploadID is not unique")
	}
	// UUIDv7 sorts by time, which is why it is used for locally minted IDs.
	if store.UploadID() < up {
		t.Error("UUIDv7 upload IDs are not time-ordered")
	}
	a := store.AuditID()
	if strings.Contains(a, "_") {
		t.Errorf("AuditID = %s; audit IDs carry no typed prefix (section 4.1)", a)
	}
	if len(a) != 36 || store.AuditID() == a {
		t.Errorf("AuditID = %s, want a fresh UUID", a)
	}
}
