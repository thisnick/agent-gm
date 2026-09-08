package cli_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thisnick/agent-gm/internal/cli"
)

func TestAdminListsHideInactiveEntries(t *testing.T) {
	now := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	past, future := now.Add(-time.Hour).Format(time.RFC3339), now.Add(time.Hour).Format(time.RFC3339)
	for _, command := range []string{"enrollment-codes", "clients", "authorization-requests"} {
		rows := []map[string]any{
			{"id": "entry_live", "expires_at": future, "status": "pending"},
			{"id": "entry_expired", "expires_at": past, "status": "pending"},
			{"id": "entry_boundary", "expires_at": now.Format(time.RFC3339), "status": "pending"},
		}
		visible := []string{"entry_live"}
		switch command {
		case "enrollment-codes":
			rows = append(rows, map[string]any{"id": "entry_used", "expires_at": future, "consumed_at": past}, map[string]any{"id": "entry_revoked", "expires_at": future, "revoked_at": past})
		case "clients":
			rows = append(rows, map[string]any{"id": "entry_activated", "expires_at": past, "activated_at": past}, map[string]any{"id": "entry_permanent", "expires_at": nil})
			visible = append(visible, "entry_activated", "entry_permanent")
		case "authorization-requests":
			rows = append(rows, map[string]any{"id": "entry_approved", "expires_at": future, "status": "approved"}, map[string]any{"id": "entry_denied", "expires_at": future, "status": "denied"}, map[string]any{"id": "entry_completed", "expires_at": past, "status": "completed"})
			visible = append(visible, "entry_approved")
		}
		for _, format := range []string{"table", "json", "jsonl"} {
			for _, all := range []bool{false, true} {
				t.Run(command+"/"+format+"/all="+map[bool]string{true: "true", false: "false"}[all], func(t *testing.T) {
					s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.URL.Path != "/v1/admin/"+command || r.URL.RawQuery != "" {
							t.Errorf("unexpected request: %s", r.URL)
						}
						w.Header().Set("Content-Type", "application/json")
						_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"items": rows, "extra": "preserved"}, "warnings": []string{}, "next_cursor": nil, "request_id": "req_test"})
					}))
					defer s.Close()
					args := []string{"admin", command, "list", "--output", format}
					if all {
						args = append(args, "--all")
					}
					env := map[string]string{"AGENT_GM_URL": s.URL, "AGENT_GM_ACCESS_TOKEN": "agm_at_test", "AGENT_GM_CREDENTIALS_FILE": filepath.Join(t.TempDir(), "credentials.json")}
					var out, errOut strings.Builder
					code := cli.Run(cli.Env{Args: args, Stdout: &out, Stderr: &errOut, Getenv: func(k string) string { return env[k] }, HTTP: s.Client(), Now: func() time.Time { return now }})
					if code != 0 {
						t.Fatalf("exit %d: %s", code, errOut.String())
					}
					for _, row := range rows {
						id := row["id"].(string)
						want := all
						for _, v := range visible {
							if id == v {
								want = true
							}
						}
						if strings.Contains(out.String(), id) != want {
							t.Errorf("%s visible=%v: %s", id, want, out.String())
						}
					}
					if format == "json" && !strings.Contains(out.String(), "preserved") {
						t.Error("lost response metadata")
					}
				})
			}
		}
	}
}
