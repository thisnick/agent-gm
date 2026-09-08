package cli

import (
	"encoding/json"
	"testing"
	"time"
)

func TestAdminRequestExplicitStatusAndEmptyList(t *testing.T) {
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct{ status, data, want string }{
		{"denied", `{"items":[{"id":"recent","status":"denied","expires_at":"2026-01-03T00:00:00Z"},{"id":"old","status":"denied","expires_at":"2026-01-01T00:00:00Z"}]}`, "recent"},
		{"", `{"items":[{"id":"recent","status":"denied","expires_at":"2026-01-03T00:00:00Z"}]}`, ""},
		{"", `{"items":[]}`, ""},
	} {
		got, err := filterAdminList(json.RawMessage(tc.data), "admin_authorization_requests_list", tc.status, now)
		if err != nil {
			t.Fatal(err)
		}
		var result struct {
			Items []struct {
				ID string `json:"id"`
			} `json:"items"`
		}
		if err := json.Unmarshal(got, &result); err != nil {
			t.Fatal(err)
		}
		if tc.want == "" {
			if result.Items == nil || len(result.Items) != 0 {
				t.Fatalf("want empty array: %s", got)
			}
		} else if len(result.Items) != 1 || result.Items[0].ID != tc.want {
			t.Fatalf("unexpected result: %s", got)
		}
	}
}
