package gm_test

import (
	"testing"

	"github.com/thisnick/agent-gm/internal/gm"
)

// Spec section 13.2: the delivery-state mapping is a table test over every
// MessageStatusType value. The enumeration against the pinned proto lives in
// the fixture-validation job (section 13.4 assertion 3); this test asserts
// the mapping itself, value by value, so a wrong entry fails without a clone.
func TestDeliveryStateMapping(t *testing.T) {
	cases := map[int32]gm.DeliveryState{
		0: gm.DeliveryStateUnknown,

		3: gm.DeliveryStateSending, 4: gm.DeliveryStateSending, 5: gm.DeliveryStateSending,
		6: gm.DeliveryStateSending, 7: gm.DeliveryStateSending, 10: gm.DeliveryStateSending,
		16: gm.DeliveryStateSending, 20: gm.DeliveryStateSending,

		1: gm.DeliveryStateSent, 14: gm.DeliveryStateSent,

		2: gm.DeliveryStateDelivered,
		11: gm.DeliveryStateRead,

		8: gm.DeliveryStateFailed, 9: gm.DeliveryStateFailed, 13: gm.DeliveryStateFailed,
		17: gm.DeliveryStateFailed, 18: gm.DeliveryStateFailed, 19: gm.DeliveryStateFailed,
		21: gm.DeliveryStateFailed, 22: gm.DeliveryStateFailed, 24: gm.DeliveryStateFailed,
		25: gm.DeliveryStateFailed, 26: gm.DeliveryStateFailed, 27: gm.DeliveryStateFailed,

		12: gm.DeliveryStateCanceled, 15: gm.DeliveryStateCanceled,

		23: gm.DeliveryStateDeleted, 117: gm.DeliveryStateDeleted, 300: gm.DeliveryStateDeleted,

		100: gm.DeliveryStateReceived, 108: gm.DeliveryStateReceived,
		109: gm.DeliveryStateReceived, 116: gm.DeliveryStateReceived,

		101: gm.DeliveryStateDownloading, 102: gm.DeliveryStateDownloading,
		103: gm.DeliveryStateDownloading, 104: gm.DeliveryStateDownloading,
		105: gm.DeliveryStateDownloading, 115: gm.DeliveryStateDownloading,

		106: gm.DeliveryStateDownloadFailed, 107: gm.DeliveryStateDownloadFailed,
		110: gm.DeliveryStateDownloadFailed, 111: gm.DeliveryStateDownloadFailed,
		112: gm.DeliveryStateDownloadFailed, 113: gm.DeliveryStateDownloadFailed,
		114: gm.DeliveryStateDownloadFailed, 118: gm.DeliveryStateDownloadFailed,
	}

	for raw, want := range cases {
		got, ok := gm.DeliveryStateFor(raw)
		if !ok {
			t.Errorf("status %d is unmapped", raw)
			continue
		}
		if got != want {
			t.Errorf("status %d maps to %s, want %s", raw, got, want)
		}
	}

	// The mapping names exactly the values above -- nothing extra, nothing
	// missing. A value added to the map without a row here fails.
	if len(gm.MappedStatusValues()) != len(cases) {
		t.Errorf("the mapping names %d values, this table names %d",
			len(gm.MappedStatusValues()), len(cases))
	}
	for _, raw := range gm.MappedStatusValues() {
		if _, ok := cases[raw]; !ok {
			t.Errorf("the mapping names status %d, which this table does not", raw)
		}
	}
}

// System events (200-279) do not get a delivery state of their own: they are
// stored with kind='system' and carry `received` for schema uniformity.
// "tombstone" is Matrix vocabulary and appears on no surface.
func TestSystemEventBand(t *testing.T) {
	for raw := int32(200); raw <= 279; raw++ {
		if gm.KindForStatus(raw) != gm.MessageKindSystem {
			t.Fatalf("status %d is not classified system", raw)
		}
		st, ok := gm.DeliveryStateFor(raw)
		if !ok || st != gm.DeliveryStateReceived {
			t.Fatalf("status %d: state = %s ok = %v", raw, st, ok)
		}
	}
	for _, raw := range []int32{0, 1, 100, 199, 280, 300} {
		if gm.KindForStatus(raw) != gm.MessageKindMessage {
			t.Errorf("status %d must not be classified system", raw)
		}
	}
	// MESSAGE_DELETED sits outside every range.
	if gm.IsSystemEventStatus(gm.MessageDeletedStatus) {
		t.Error("MESSAGE_DELETED(300) must not be in the system band")
	}
}

// A value added upstream after this pin falls to unknown, and says so.
func TestUnmappedStatusIsReportedNotSwallowed(t *testing.T) {
	st, ok := gm.DeliveryStateFor(999)
	if ok {
		t.Fatal("status 999 must not be reported as mapped")
	}
	if st != gm.DeliveryStateUnknown {
		t.Errorf("an unmapped status renders as %s, want unknown", st)
	}
}

// Spec section 4.4: a forward skip is accepted, a backward move is refused.
func TestTransitionRules(t *testing.T) {
	allowed := [][2]gm.DeliveryState{
		{gm.DeliveryStateSending, gm.DeliveryStateSent},
		{gm.DeliveryStateSent, gm.DeliveryStateDelivered},
		{gm.DeliveryStateDelivered, gm.DeliveryStateRead},
		{gm.DeliveryStateSending, gm.DeliveryStateDelivered}, // a forward skip
		{gm.DeliveryStateSending, gm.DeliveryStateRead},      // a bigger skip
		{gm.DeliveryStateSending, gm.DeliveryStateFailed},
		{gm.DeliveryStateSent, gm.DeliveryStateFailed},
		{gm.DeliveryStateSending, gm.DeliveryStateCanceled},
		{gm.DeliveryStateRead, gm.DeliveryStateDeleted}, // any -> deleted
		{gm.DeliveryStateFailed, gm.DeliveryStateDeleted},
		{gm.DeliveryStateUnknown, gm.DeliveryStateRead}, // a late authoritative status
		{gm.DeliveryStateUnknown, gm.DeliveryStateFailed},
		{gm.DeliveryStateSent, gm.DeliveryStateSent}, // idempotent
	}
	for _, tc := range allowed {
		if !gm.TransitionAllowed(tc[0], tc[1]) {
			t.Errorf("%s -> %s must be allowed", tc[0], tc[1])
		}
	}

	refused := [][2]gm.DeliveryState{
		{gm.DeliveryStateRead, gm.DeliveryStateSent},      // the backward move
		{gm.DeliveryStateDelivered, gm.DeliveryStateSent}, // ditto
		{gm.DeliveryStateSent, gm.DeliveryStateSending},
		{gm.DeliveryStateRead, gm.DeliveryStateDelivered},
		{gm.DeliveryStateDelivered, gm.DeliveryStateFailed}, // failed only from sending|sent
		{gm.DeliveryStateSent, gm.DeliveryStateCanceled},    // canceled only from sending
		{gm.DeliveryStateDeleted, gm.DeliveryStateSent},
	}
	for _, tc := range refused {
		if gm.TransitionAllowed(tc[0], tc[1]) {
			t.Errorf("%s -> %s must be refused", tc[0], tc[1])
		}
	}
}

// The ignore set is carried over verbatim from upstream. This test asserts
// the DM-only arm really is DM-only; the fixture-validation job asserts the
// two sets still agree with the pinned tree.
func TestShouldIgnoreStatus(t *testing.T) {
	// TOMBSTONE_ONE_ON_ONE_SMS_CREATED(206) is ignored in a direct
	// conversation and kept in a group.
	if !gm.ShouldIgnoreStatus(206, true) {
		t.Error("206 must be ignored in a DM")
	}
	if gm.ShouldIgnoreStatus(206, false) {
		t.Error("206 must be kept in a group")
	}
	// TOMBSTONE_RCS_GROUP_CREATED(203) is ignored outright.
	if !gm.ShouldIgnoreStatus(203, true) || !gm.ShouldIgnoreStatus(203, false) {
		t.Error("203 must be ignored in both")
	}
	// An ordinary message is never ignored.
	if gm.ShouldIgnoreStatus(1, true) || gm.ShouldIgnoreStatus(100, false) {
		t.Error("ordinary statuses must not be ignored")
	}
}

// Direction is derived from the status band.
func TestDirectionFromStatusBand(t *testing.T) {
	for _, raw := range []int32{1, 2, 5, 11, 27} {
		if got := (gm.Message{StatusRaw: raw}).Direction(); got != gm.DirectionOutgoing {
			t.Errorf("status %d is %s, want outgoing", raw, got)
		}
	}
	for _, raw := range []int32{100, 108, 116, 118} {
		if got := (gm.Message{StatusRaw: raw}).Direction(); got != gm.DirectionIncoming {
			t.Errorf("status %d is %s, want incoming", raw, got)
		}
	}
}

// Spec section 4.6: force_rcs is true exactly when the conversation is RCS
// and its send mode is SEND_MODE_AUTO. The raw mode is never served.
func TestForceRCSEligible(t *testing.T) {
	cases := []struct {
		typ  gm.ConversationType
		mode gm.SendMode
		want bool
	}{
		{gm.ConversationTypeRCS, gm.SendModeAuto, true},
		{gm.ConversationTypeRCS, gm.SendModeXMS, false},
		{gm.ConversationTypeRCS, gm.SendModeXMSLatch, false},
		{gm.ConversationTypeSMSMMS, gm.SendModeAuto, false},
		{gm.ConversationTypeUnknown, gm.SendModeAuto, false},
	}
	for _, tc := range cases {
		c := gm.Conversation{Type: tc.typ, SendModeRaw: tc.mode}
		if got := c.ForceRCSEligible(); got != tc.want {
			t.Errorf("%s/%s: force_rcs = %v, want %v", tc.typ, tc.mode, got, tc.want)
		}
	}
}

// Reactions are a closed enum, and normalisation is mandatory: adding one
// spelling of a heart and removing the other must be the same reaction.
func TestEmojiCanonicalisation(t *testing.T) {
	t1, e1 := gm.CanonicaliseEmojiInput("❤")             // bare heart
	t2, e2 := gm.CanonicaliseEmojiInput("❤️")        // with variation selector
	if t1 != gm.EmojiTypeRedHeart || t2 != gm.EmojiTypeRedHeart {
		t.Fatalf("both spellings must be RED_HEART, got %s and %s", t1, t2)
	}
	if e1 == nil || e2 == nil || *e1 != *e2 {
		t.Fatalf("the two spellings canonicalise differently: %v and %v", e1, e2)
	}
	if *e1 != "❤️" {
		t.Errorf("the canonical heart is %q, upstream renders the variation-selector form", *e1)
	}

	// Anything outside the eleven becomes CUSTOM and keeps the caller's
	// unicode.
	tc, ec := gm.CanonicaliseEmojiInput("\U0001F984")
	if tc != gm.EmojiTypeCustom {
		t.Errorf("an unrecognised emoji is %s, want custom", tc)
	}
	if ec == nil || *ec != "\U0001F984" {
		t.Errorf("a custom reaction keeps the caller's unicode, got %v", ec)
	}

	// A type with no unicode of its own serves emoji: null plus a type,
	// rather than being silently skipped the way upstream does.
	if got := gm.CanonicalEmoji(gm.EmojiTypeEmotify, ""); got != nil {
		t.Errorf("EMOTIFY has no emoji, got %q", *got)
	}
	if got := gm.CanonicalEmoji(gm.EmojiTypeUnspecified, ""); got != nil {
		t.Errorf("REACTION_TYPE_UNSPECIFIED has no emoji, got %q", *got)
	}

	// All fourteen values have a name.
	for raw := int32(0); raw <= 13; raw++ {
		if _, ok := gm.EmojiTypeForRaw(raw); !ok {
			t.Errorf("EmojiType %d has no name", raw)
		}
	}
	if _, ok := gm.EmojiTypeForRaw(99); ok {
		t.Error("an unrecognised EmojiType must be reported as unrecognised")
	}
}

// The tmp ID is a bare UUID, not an op_-prefixed operation ID (D22).
func TestGenerateTmpIDIsABareUUID(t *testing.T) {
	id := gm.GenerateTmpID()
	if len(id) != 36 {
		t.Fatalf("tmp ID %q is not 36 characters", id)
	}
	for _, prefix := range []string{"op_", "msg_", "conv_"} {
		if len(id) > len(prefix) && id[:len(prefix)] == prefix {
			t.Errorf("tmp ID %q carries the %q prefix; it must be a bare UUID", id, prefix)
		}
	}
	if gm.GenerateTmpID() == id {
		t.Error("a tmp ID is minted per send attempt, so two must differ")
	}
}
