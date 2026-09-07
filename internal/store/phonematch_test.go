package store_test

import (
	"context"
	"testing"

	"github.com/thisnick/agent-gm/internal/gm"
	"github.com/thisnick/agent-gm/internal/store"
)

// Live-gate finding 4: `conversations list --participant` matched only
// E.164. Section 7.6 promises "an E.164 number (+12025550123), the bare
// digits, a national form", and the two forms a human actually types
// returned an empty page -- which is a valid answer, so nothing looked
// wrong.
//
// Plant: drop the suffix arm from the predicate and the bare-digit and
// national cases fail. Planted 2026-09-07.
func TestParticipantAndSenderAcceptAllThreePhoneForms(t *testing.T) {
	ctx := context.Background()
	st, _ := newStore(t)
	id := seedAccount(t, st, "phones@example.com")
	seedThread(t, st, id, "conv-phones", "Alex", "+12025550123", false,
		gm.FolderInbox, "one", "two")

	for _, form := range []string{
		"+12025550123",   // E.164
		"12025550123",    // bare digits with the country code
		"2025550123",     // bare national digits
		"(202) 555-0123", // national, as a human writes it
		"202-555-0123",
		"202 555 0123",
	} {
		t.Run(form, func(t *testing.T) {
			_, phone, err := st.ResolveParticipant(ctx, form)
			if err != nil {
				t.Fatal(err)
			}
			rows, err := st.ListConversations(ctx, store.ConversationQuery{AllAccounts: true,
				ParticipantPhone: phone,
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) == 0 {
				t.Errorf("participant=%q matched no conversation, though the thread "+
					"holds +12025550123", form)
			}
		})
	}

	// A different number does not match, so the suffix rule is a filter
	// rather than a wildcard.
	_, phone, err := st.ResolveParticipant(ctx, "2025550999")
	if err != nil {
		t.Fatal(err)
	}
	rows, err := st.ListConversations(ctx, store.ConversationQuery{AllAccounts: true,
		ParticipantPhone: phone,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Errorf("a different number matched %d conversations", len(rows))
	}
}

// A fragment too short to identify anybody matches on the exact value only.
// A filter that matches half the address book is not a filter.
func TestPhoneMatchRefusesAShortFragment(t *testing.T) {
	for _, tc := range []struct{ raw, wantSuffix string }{
		{"+12025550123", "2025550123"},
		{"2025550123", "2025550123"},
		{"(202) 555-0123", "2025550123"},
		{"5550123", "5550123"},
		{"0123", ""},
		{"", ""},
		{"+442071838750", "2071838750"},
	} {
		exact, suffix := store.PhoneMatch(tc.raw)
		if exact != tc.raw {
			t.Errorf("PhoneMatch(%q) exact = %q", tc.raw, exact)
		}
		if suffix != tc.wantSuffix {
			t.Errorf("PhoneMatch(%q) suffix = %q, want %q", tc.raw, suffix, tc.wantSuffix)
		}
	}
}
