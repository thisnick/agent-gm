package mcp_test

import (
	"context"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/thisnick/agent-gm/internal/gm"
)

func TestConversationFiltersThroughMCP(t *testing.T) {
	h := newHarness(t)
	one, two := h.addAccount(addressA), h.addAccount(addressB)
	base := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	seed := func(account, name string, folder gm.Folder, kind gm.ConversationType, group, pinned, unread bool, at time.Time, peer string) string {
		t.Helper()
		id, err := h.Store.UpsertConversation(context.Background(), account, gm.Conversation{
			SourceID: name, Name: name, Folder: folder, Type: kind, IsGroup: group,
			Pinned: pinned, Unread: unread, LastActivity: at,
			Participants: []gm.Participant{
				{SourceID: name + "-peer", DisplayName: peer, PhoneE164: fictionalA, FormattedNumber: "(202) 555-0101"},
				{SourceID: name + "-peer-two", DisplayName: peer},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	a := seed(one, "First thread", gm.FolderInbox, gm.ConversationTypeRCS, false, true, true, base, "Alex_100%")
	b := seed(one, "Second thread", gm.FolderArchive, gm.ConversationTypeSMSMMS, true, false, false, base.Add(time.Hour), "Blair")
	c := seed(two, "Third thread", gm.FolderSpamBlocked, gm.ConversationTypeUnknown, false, true, false, base.Add(2*time.Hour), "Alex_100%")
	d := seed(one, "Fourth thread", gm.FolderInbox, gm.ConversationTypeRCS, false, true, false, base, "AlexX100Z")
	tied := []string{a, d}
	sort.Sort(sort.Reverse(sort.StringSlice(tied)))
	all := append([]string{c, b}, tied...)
	cases := []struct {
		name string
		args map[string]any
		want []string
	}{
		{"default order", nil, all},
		{"account", map[string]any{"account_id": one}, append([]string{b}, tied...)},
		{"thread name", map[string]any{"query": "FIRST"}, []string{a}},
		{"participant name literal wildcards", map[string]any{"query": "alex_100%"}, []string{c, a}},
		{"participant name partial", map[string]any{"query": "lai"}, []string{b}},
		{"participant number", map[string]any{"query": "202555"}, all},
		{"formatted number", map[string]any{"query": "(202)"}, all},
		{"groups", map[string]any{"group_only": true}, []string{b}},
		{"direct", map[string]any{"group_only": false}, append([]string{c}, tied...)},
		{"null group", map[string]any{"group_only": nil}, all},
		{"unread", map[string]any{"unread_only": true}, []string{a}},
		{"false unread has no filter", map[string]any{"unread_only": false}, all},
		{"pinned", map[string]any{"pinned_only": true}, append([]string{c}, tied...)},
		{"false pinned has no filter", map[string]any{"pinned_only": false}, all},
		{"active", map[string]any{"folder": "active"}, tied},
		{"legacy inbox", map[string]any{"folder": "inbox"}, tied},
		{"legacy spam", map[string]any{"folder": "spam"}, []string{c}},
		{"archived", map[string]any{"folder": "archived"}, []string{b}},
		{"spam blocked", map[string]any{"folder": "spam_blocked"}, []string{c}},
		{"rcs", map[string]any{"type": "rcs"}, tied},
		{"sms", map[string]any{"type": "sms_mms"}, []string{b}},
		{"unknown", map[string]any{"type": "unknown"}, []string{c}},
		{"inclusive after", map[string]any{"after": base.Add(time.Hour).Format(time.RFC3339)}, []string{c, b}},
		{"inclusive before", map[string]any{"before": base.Format(time.RFC3339)}, tied},
		{"equal endpoints", map[string]any{"after": base.Format(time.RFC3339), "before": base.Format(time.RFC3339)}, tied},
		{"offset timestamp", map[string]any{"before": "2026-09-08T05:00:00-07:00"}, tied},
		{"epoch bound", map[string]any{"before": "1970-01-01T00:00:00Z"}, []string{}},
		{"combined", map[string]any{"account_id": one, "query": "alex_100%", "participant": fictionalA, "folder": "active", "type": "rcs", "group_only": false, "pinned_only": true, "unread_only": true, "after": base.Format(time.RFC3339), "before": base.Format(time.RFC3339)}, []string{a}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := h.tool("list_conversations", tc.args)
			if isError(result) {
				t.Fatalf("filter failed: %v", resultError(t, result))
			}
			got := []string{}
			for _, row := range itemsOf(t, result) {
				got = append(got, row["id"].(string))
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
	for _, args := range []map[string]any{
		{"after": "yesterday"}, {"before": "2026-09-08"},
		{"after": base.Add(time.Hour).Format(time.RFC3339), "before": base.Format(time.RFC3339)},
		{"pinned_only": "yes"}, {"group_only": "yes"}, {"folder": "not-a-folder"}, {"type": "not-a-type"},
	} {
		result := h.tool("list_conversations", args)
		if !isError(result) {
			t.Fatalf("accepted invalid filters %v", args)
		}
	}

	// Pagination must retain all filters, including explicit false and tied timestamps.
	args := map[string]any{"group_only": false, "pinned_only": true, "after": base.Format(time.RFC3339), "limit": 1}
	got := []string{}
	for i := 0; i < 5; i++ {
		result := h.tool("list_conversations", args)
		for _, row := range itemsOf(t, result) {
			got = append(got, row["id"].(string))
		}
		cursor, _ := structured(t, result)["next_cursor"].(string)
		if cursor == "" {
			break
		}
		args["cursor"] = cursor
		changed := map[string]any{"group_only": false, "pinned_only": false, "after": base.Format(time.RFC3339), "limit": 1, "cursor": cursor}
		if !isError(h.tool("list_conversations", changed)) {
			t.Fatal("cursor allowed a changed pinned filter")
		}
	}
	want := append([]string{c}, tied...)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("pagination got %v, want %v", got, want)
	}
}

// The folder vocabulary is also used by updates; only active/archived are writable.
func TestConversationWritableFolders(t *testing.T) {
	h := newHarness(t)
	account := h.addAccount(addressA)
	conv := h.seedConversation(account, "folder-test")
	for _, folder := range []string{"archived", "active"} {
		result := h.tool("update_conversation", map[string]any{"conversation_id": conv.ID, "folder": folder})
		if isError(result) {
			t.Fatalf("folder %s: %v", folder, resultError(t, result))
		}
	}
}
