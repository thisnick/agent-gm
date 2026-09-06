package fake_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/thisnick/agent-gm/internal/clock"
	"github.com/thisnick/agent-gm/internal/gm"
	"github.com/thisnick/agent-gm/internal/gm/fake"
)

// Slice 1 acceptance test 6, first half: the fake satisfies the whole Backend
// interface, asserted at compile time.
var _ gm.Backend = (*fake.Backend)(nil)

func newPaired(t *testing.T, address string, opts ...fake.Option) *fake.Backend {
	t.Helper()
	b := fake.New(address, opts...)
	if _, err := b.StartGooglePairing(context.Background(), testCookies(), 0, func(string) {}); err != nil {
		t.Fatalf("pairing: %v", err)
	}
	drain(b)
	return b
}

func testCookies() map[string]string {
	// Nonfunctional placeholders. No real cookie value ever enters this
	// repository (spec sections 12.2, 13.4).
	return map[string]string{
		"SID": "fixture", "HSID": "fixture", "OSID": "fixture",
		"SSID": "fixture", "APISID": "fixture", "SAPISID": "fixture",
		"__Secure-1PSIDTS": "fixture",
	}
}

func drain(b *fake.Backend) []gm.Event {
	var out []gm.Event
	for {
		select {
		case ev := <-b.Events():
			out = append(out, ev)
		default:
			return out
		}
	}
}

// Slice 1 acceptance test 6, second half: every method is called once. If a
// method is unimplementable against the fake, it does not belong in the
// interface.
func TestEveryBackendMethodIsCallable(t *testing.T) {
	ctx := context.Background()
	b := newPaired(t, "owner@example.com")
	b.SeedConversation(gm.Conversation{
		SourceID: "c1", Type: gm.ConversationTypeRCS, SendModeRaw: gm.SendModeAuto,
		Folder: gm.FolderInbox, DefaultOutgoingID: "me", LastActivity: time.Unix(1, 0),
	})
	b.SeedMessage(gm.Message{SourceID: "m1", ConversationID: "c1", Text: "hello",
		Timestamp: time.Unix(1, 0), StatusRaw: 100, Kind: gm.MessageKindMessage,
		DeliveryState: gm.DeliveryStateReceived})
	b.SeedContacts(gm.Contact{SourceID: "p1", DisplayName: "Fixture", PhoneE164: "+12025550123"})
	b.SeedTopContacts(gm.Contact{SourceID: "p1", DisplayName: "Fixture", IsTop: true})
	b.SeedAvatar("p1", []byte{1, 2, 3})

	checks := []struct {
		name string
		call func() error
	}{
		{"Connect", func() error { return b.Connect(ctx) }},
		{"IsConnected", func() error { _ = b.IsConnected(); return nil }},
		{"IsLoggedIn", func() error {
			if !b.IsLoggedIn() {
				return errors.New("a paired fake must be logged in")
			}
			return nil
		}},
		{"SessionID", func() error {
			if b.SessionID() == "" {
				return errors.New("no session ID after connect")
			}
			return nil
		}},
		{"FetchConfig", func() error { _, err := b.FetchConfig(ctx); return err }},
		{"CompiledConfigVersion", func() error { _ = b.CompiledConfigVersion(); return nil }},
		{"IsDefaultSMSApp", func() error { _, err := b.IsDefaultSMSApp(ctx); return err }},
		{"RefreshGoogleCookies", func() error { return b.RefreshGoogleCookies(ctx, testCookies()) }},
		{"ListConversations", func() error { _, err := b.ListConversations(ctx, gm.FolderInbox, 10); return err }},
		{"GetConversation", func() error { _, err := b.GetConversation(ctx, "c1"); return err }},
		{"GetConversationType", func() error { _, err := b.GetConversationType(ctx, "c1"); return err }},
		{"ListMessages", func() error { _, _, err := b.ListMessages(ctx, "c1", 10, nil); return err }},
		{"ListContacts", func() error { _, err := b.ListContacts(ctx); return err }},
		{"ListTopContacts", func() error { _, err := b.ListTopContacts(ctx); return err }},
		{"ContactAvatars", func() error { _, err := b.ContactAvatars(ctx, []string{"p1"}); return err }},
		{"DownloadAvatar", func() error { _, err := b.DownloadAvatar(ctx, "https://example.invalid/a.png"); return err }},
		{"ResolveConversation", func() error {
			_, err := b.ResolveConversation(ctx, []string{"+12025550123"}, "")
			return err
		}},
		{"SendText", func() error {
			_, err := b.SendText(ctx, gm.SendTextRequest{ConversationID: "c1", TmpID: gm.GenerateTmpID(), Text: "hi"})
			return err
		}},
		{"SendMedia", func() error {
			_, err := b.SendMedia(ctx, gm.SendMediaRequest{ConversationID: "c1", TmpID: gm.GenerateTmpID()})
			return err
		}},
		{"React", func() error { return b.React(ctx, "m1", "\U0001F44D", gm.ReactionActionAdd) }},
		{"MarkRead", func() error { return b.MarkRead(ctx, "c1", "m1") }},
		{"SetTyping", func() error { return b.SetTyping(ctx, "c1") }},
		{"UpdateConversation", func() error {
			f := gm.FolderArchive
			return b.UpdateConversation(ctx, "c1", gm.ConversationChange{Folder: &f})
		}},
		{"DeleteMessage", func() error { return b.DeleteMessage(ctx, "m1") }},
		{"DeleteConversation", func() error { return b.DeleteConversation(ctx, "c1", "+12025550123") }},
		{"Upload", func() error { _, err := b.Upload(ctx, []byte("bytes"), "a.jpg", "image/jpeg"); return err }},
		{"RequestFullSizeImage", func() error { return b.RequestFullSizeImage(ctx, "m1", "a1") }},
		{"Events", func() error {
			if b.Events() == nil {
				return errors.New("nil event channel")
			}
			return nil
		}},
		{"Disconnect", func() error { b.Disconnect(); return nil }},
	}
	for _, c := range checks {
		if err := c.call(); err != nil {
			t.Errorf("%s: %v", c.name, err)
		}
	}

	// Download needs an ID Upload produced, so it runs last against a real
	// upload rather than a made-up ID.
	ref, err := b.Upload(ctx, []byte("payload"), "b.jpg", "image/jpeg")
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	got, err := b.Download(ctx, ref.MediaID, ref.DecryptionKey)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if string(got) != "payload" {
		t.Errorf("Download returned %q", got)
	}

	// StartGooglePairing was already exercised by newPaired.
	if b.CallCount("StartGooglePairing") != 1 {
		t.Errorf("StartGooglePairing call count = %d", b.CallCount("StartGooglePairing"))
	}
}

// The fake is scriptable to return every error in spec section 3.5, one per
// call, so the layers above it can be tested against real failures.
func TestFakeIsScriptableWithEverySection35Error(t *testing.T) {
	ctx := context.Background()
	for _, sentinel := range []error{
		gm.ErrPhoneNotResponding, gm.ErrConnectionClosed, gm.ErrInvalidCredentials,
		gm.ErrRequestedEntityNotFound, gm.ErrCallerNoPermission,
		gm.RequestError{Type: 9, Message: "resource exhausted"},
		gm.HTTPError{Action: "sending", StatusCode: 502},
	} {
		b := newPaired(t, "owner@example.com")
		b.ScriptErrors(sentinel)
		_, err := b.SendText(ctx, gm.SendTextRequest{ConversationID: "c1", TmpID: gm.GenerateTmpID(), Text: "hi"})
		if err == nil {
			t.Fatalf("%v: expected the scripted error", sentinel)
		}
		var ge *gm.Error
		if !errors.As(err, &ge) {
			t.Fatalf("%v: the fake must return a classified error, got %T", sentinel, err)
		}
		if ge.Code == gm.CodeInternalError {
			t.Errorf("%v classified as internal_error", sentinel)
		}
	}
}

// Each GaiaPairingErrorCode outcome, and the two Agent GM adds.
func TestFakePairingFailures(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		err  error
		code gm.Code
	}{
		{gm.ErrIncorrectEmoji, gm.CodePairingWrongEmoji},
		{gm.ErrPairingCancelled, gm.CodePairingCancelled},
		{gm.ErrPairingTimeout, gm.CodePairingTimeout},
		{gm.ErrPairingInitTimeout, gm.CodePairingInitTimeout},
	} {
		b := fake.New("owner@example.com")
		b.ScriptPairError(tc.err)
		_, err := b.StartGooglePairing(ctx, testCookies(), 0, func(string) {})
		var ge *gm.Error
		if !errors.As(err, &ge) || ge.Code != tc.code {
			t.Errorf("%v: got %v, want %s", tc.err, err, tc.code)
		}
		if b.IsLoggedIn() {
			t.Errorf("%v: a failed pairing must not leave the fake logged in", tc.err)
		}
	}

	// Missing cookies, and a set scoped to .google.com alone -- which is the
	// most common way the real flow fails, because OSID is host-scoped to
	// messages.google.com.
	b := fake.New("owner@example.com")
	partial := testCookies()
	delete(partial, "OSID")
	_, err := b.StartGooglePairing(ctx, partial, 0, nil)
	var ge *gm.Error
	if !errors.As(err, &ge) || ge.Code != gm.CodePairingNoCookies {
		t.Fatalf("a capture without OSID must be refused, got %v", err)
	}
	missing, _ := ge.Details["missing_cookies"].([]string)
	if len(missing) != 1 || missing[0] != "OSID" {
		t.Errorf("details.missing_cookies = %v, want [OSID]", ge.Details["missing_cookies"])
	}

	// Zero primary devices.
	none := fake.New("owner@example.com", fake.WithDevices())
	_, err = none.StartGooglePairing(ctx, testCookies(), 0, nil)
	if !errors.As(err, &ge) || ge.Code != gm.CodePairingNoDevices {
		t.Errorf("zero devices must be pairing_no_devices, got %v", err)
	}

	// An empty address is refused before anything is created.
	blank := fake.New("")
	_, err = blank.StartGooglePairing(ctx, testCookies(), 0, nil)
	if !errors.As(err, &ge) || ge.Code != gm.CodePairingNoAccount {
		t.Errorf("an empty address must be pairing_no_account, got %v", err)
	}
}

// Several primary-looking devices: newest first, then device_index selects
// among them. The library does not error on several.
func TestFakeDeviceSelection(t *testing.T) {
	ctx := context.Background()
	older := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	mk := func() *fake.Backend {
		return fake.New("owner@example.com", fake.WithDevices(
			fake.Device{RegUUID: "old", LastSeen: older},
			fake.Device{RegUUID: "new", LastSeen: newer},
		))
	}
	dev, err := mk().StartGooglePairing(ctx, testCookies(), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if dev.DestRegUUID != "new" {
		t.Errorf("device_index 0 chose %s, want the most recently seen", dev.DestRegUUID)
	}
	if dev.DeviceCount != 2 {
		t.Errorf("device count = %d, want 2", dev.DeviceCount)
	}
	dev, err = mk().StartGooglePairing(ctx, testCookies(), 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if dev.DestRegUUID != "old" {
		t.Errorf("device_index 1 chose %s, want the next one", dev.DestRegUUID)
	}
}

// A cookie refresh against a different Google account is refused with
// pairing_wrong_account and changes nothing. This is stricter than upstream
// on purpose (spec section 3.2).
func TestFakeWrongAccountRefreshChangesNothing(t *testing.T) {
	ctx := context.Background()
	b := newPaired(t, "owner@example.com")
	b.ScriptRefreshAddress("someone-else@example.com")
	err := b.RefreshGoogleCookies(ctx, testCookies())
	var ge *gm.Error
	if !errors.As(err, &ge) || ge.Code != gm.CodePairingWrongAccount {
		t.Fatalf("got %v, want pairing_wrong_account", err)
	}
	if !b.IsLoggedIn() {
		t.Error("a refused refresh must change nothing: the account stays logged in")
	}
	if b.Address() != "owner@example.com" {
		t.Errorf("the address changed to %s", b.Address())
	}
}

// ListMessages pages with a real cursor, including the equal-timestamp case:
// Google timestamps collide, so the message ID is the tiebreaker and it is
// part of the cursor.
func TestFakeCursorsHandleEqualTimestamps(t *testing.T) {
	ctx := context.Background()
	b := newPaired(t, "owner@example.com")
	b.SeedConversation(gm.Conversation{SourceID: "c1", Folder: gm.FolderInbox})
	same := time.Unix(1757000000, 0).UTC()
	for _, id := range []string{"m1", "m2", "m3", "m4"} {
		b.SeedMessage(gm.Message{SourceID: id, ConversationID: "c1", Timestamp: same,
			StatusRaw: 100, Kind: gm.MessageKindMessage, DeliveryState: gm.DeliveryStateReceived})
	}
	var seen []string
	var cursor *gm.Cursor
	for range 10 {
		page, next, err := b.ListMessages(ctx, "c1", 2, cursor)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range page {
			seen = append(seen, m.SourceID)
		}
		if next == nil {
			break
		}
		cursor = next
	}
	want := []string{"m4", "m3", "m2", "m1"}
	if len(seen) != len(want) {
		t.Fatalf("paging returned %v, want %v", seen, want)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("paging returned %v, want %v", seen, want)
		}
	}
}

// A dropped event increments dropped_events rather than stalling the poller.
func TestFakeDropsEventsAndCountsThem(t *testing.T) {
	b := fake.New("owner@example.com")
	b.SetDropEvents(true)
	before := b.DroppedEvents()
	b.Emit(&gm.EventNoDataReceived{})
	if b.DroppedEvents() != before+1 {
		t.Errorf("dropped_events = %d, want %d", b.DroppedEvents(), before+1)
	}
	if len(drain(b)) != 0 {
		t.Error("a dropped event must not be delivered")
	}
}

// The library's dedup abandons every remaining part of a batch on a hit. The
// fake reproduces that, so the reconciliation sweep is exercised against real
// loss rather than a stub.
func TestFakeAbandonsTheRestOfABatch(t *testing.T) {
	b := fake.New("owner@example.com")
	before := b.DroppedEvents()
	b.EmitBatch(1,
		&gm.EventMessage{Message: gm.Message{SourceID: "m1", ConversationID: "c1"}},
		&gm.EventMessage{Message: gm.Message{SourceID: "m2", ConversationID: "c1"}},
		&gm.EventMessage{Message: gm.Message{SourceID: "m3", ConversationID: "c1"}},
	)
	got := drain(b)
	if len(got) != 1 {
		t.Fatalf("the batch delivered %d events, want 1 before it was abandoned", len(got))
	}
	if b.DroppedEvents() != before+2 {
		t.Errorf("dropped_events = %d, want %d", b.DroppedEvents(), before+2)
	}
}

// FetchConfig is scriptable with a live ConfigVersion differing from the
// compiled one, so config_version_stale is testable with no phone. The
// detection rule is the version diff alone.
func TestFakeScriptsAStaleConfigVersion(t *testing.T) {
	ctx := context.Background()
	b := newPaired(t, "owner@example.com")
	stale := gm.ConfigVersion{Year: 2026, Month: 3, Day: 18, V1: 4, V2: 6}
	b.SetLiveConfigVersion(stale)
	info, err := b.FetchConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info.Live.SameDate(b.CompiledConfigVersion()) {
		t.Fatal("the scripted live version must differ from the compiled one")
	}
	// A non-SUCCESS status plus a version diff is config_version_stale...
	b.ScriptResolveStatuses(gm.ResolveStatus(4))
	res, err := b.ResolveConversation(ctx, []string{"+12025550123"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status == gm.ResolveStatusSuccess {
		t.Fatal("the scripted status must not be SUCCESS")
	}
	// ...and the converse: matching versions make the same status
	// google_undocumented_status, not config_version_stale.
	b.SetLiveConfigVersion(b.CompiledConfigVersion())
	info, err = b.FetchConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Live.SameDate(b.CompiledConfigVersion()) {
		t.Fatal("the versions must match now")
	}
}

// CREATE_RCS is retried exactly once, exactly as upstream does. The fake
// consumes the retry's scripted status.
func TestFakeResolveStatusScript(t *testing.T) {
	ctx := context.Background()
	b := newPaired(t, "owner@example.com")
	b.ScriptResolveStatuses(gm.ResolveStatusCreateRCS, gm.ResolveStatusSuccess)
	res, err := b.ResolveConversation(ctx, []string{"+12025550123", "+12025550124"}, "Fixture group")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != gm.ResolveStatusSuccess {
		t.Errorf("status = %v, want SUCCESS after the single retry", res.Status)
	}
	if res.Conversation == nil {
		t.Fatal("no conversation in the result")
	}
}

// The fake takes a clock, and each fake takes its own: a test can advance one
// account's clock while the other's stands still. No test sleeps.
func TestEachFakeCanHoldItsOwnClock(t *testing.T) {
	a := clock.NewFake()
	c := clock.NewFake()
	fa := fake.New("a@example.com", fake.WithClock(a))
	fb := fake.New("b@example.com", fake.WithClock(c))
	start := fa.Clock().Now()
	a.Advance(time.Hour)
	if !fa.Clock().Now().Equal(start.Add(time.Hour)) {
		t.Error("advancing A's clock must move A")
	}
	if !fb.Clock().Now().Equal(start) {
		t.Error("advancing A's clock must not move B")
	}
}

// The send script walks each SendMessageResponse.Status, including
// FAILURE_2 twice then success, which exercises the retry backoff.
func TestFakeSendStatusScript(t *testing.T) {
	ctx := context.Background()
	b := newPaired(t, "owner@example.com")
	b.SeedConversation(gm.Conversation{SourceID: "c1", Folder: gm.FolderInbox, DefaultOutgoingID: "me"})
	b.ScriptSendStatuses(gm.SendStatusFailure2, gm.SendStatusFailure2, gm.SendStatusSuccess)
	want := []gm.SendStatus{gm.SendStatusFailure2, gm.SendStatusFailure2, gm.SendStatusSuccess}
	for i, w := range want {
		res, err := b.SendText(ctx, gm.SendTextRequest{ConversationID: "c1", TmpID: gm.GenerateTmpID(), Text: "hi"})
		if err != nil {
			t.Fatalf("attempt %d: %v", i, err)
		}
		if res.Status != w {
			t.Errorf("attempt %d: status = %s, want %s", i, res.Status, w)
		}
	}
	if got := gm.SendFailure(gm.SendStatusFailure4).Code; got != gm.CodeNotDefaultSMSApp {
		t.Errorf("FAILURE_4 maps to %s", got)
	}
}
