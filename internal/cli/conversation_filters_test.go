package cli_test

import (
	"net/url"
	"testing"
)

func TestConversationFilterFlags(t *testing.T) {
	s := newStub(t)
	result := runCLI(t, s, nil, "", "conversations", "list", "--group=false", "--pinned", "--unread", "--after", "2026-09-08T00:00:00Z", "--before", "2026-09-09T00:00:00Z", "--type", "unknown", "--folder", "spam_blocked")
	if result.code != 0 {
		t.Fatalf("CLI exited %d: %s", result.code, result.stderr)
	}
	requests := s.seen()
	q, err := url.ParseQuery(requests[len(requests)-1].Query)
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{"group_only": "false", "pinned_only": "true", "unread_only": "true", "after": "2026-09-08T00:00:00Z", "before": "2026-09-09T00:00:00Z", "type": "unknown", "folder": "spam_blocked"} {
		if q.Get(key) != want {
			t.Errorf("%s = %q, want %q", key, q.Get(key), want)
		}
	}
}

func TestOmittedConversationBooleansStayAbsent(t *testing.T) {
	s := newStub(t)
	result := runCLI(t, s, nil, "", "conversations", "list")
	if result.code != 0 {
		t.Fatalf("CLI exited %d: %s", result.code, result.stderr)
	}
	requests := s.seen()
	q, err := url.ParseQuery(requests[len(requests)-1].Query)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"group_only", "pinned_only", "unread_only"} {
		if q.Has(key) {
			t.Errorf("omitted %s was sent", key)
		}
	}
}
