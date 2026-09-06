package cli_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thisnick/agent-gm/internal/apierr"
	"github.com/thisnick/agent-gm/internal/cli"
)

// A stub `/v1` server, built with httptest so it picks its own port.
//
// These tests are about the CLI, not the server: what is under test is that
// `agm` sends what the route inventory says, renders what comes back, and
// exits with the code spec section 11.2 assigns. The real server is another
// package's work, so a stub that answers the envelopes of section 7.1 is the
// honest fixture -- and it lets a test produce an answer the real server
// would take a fault injector to produce, which is exactly what the
// exit-code matrix and the effect-sentence test need.
//
// It never dials a phone and never reaches Google. Nothing here has a real
// phone number in it.
type stub struct {
	*httptest.Server

	mu       sync.Mutex
	requests []recorded

	// failure, when set, makes every route answer that error envelope.
	failure *apierr.Code
	// effect is the sentence the two delete routes return. It is settable so
	// section 16 Slice 2 test 28 can prove the CLI prints the SERVER's
	// sentence rather than one it built itself.
	effect string
	// operation is the operation object every mutation returns.
	operationStatus string
	// deliveryState is what GET /v1/messages/{id} reports.
	deliveryState string
	// conversationType is what GET /v1/conversations/{id} reports.
	conversationType string
	// refreshRejects makes POST /v1/auth/refresh answer invalid_token.
	refreshRejects bool
}

type recorded struct {
	Method         string
	Path           string
	Query          string
	Body           string
	Authorization  string
	IdempotencyKey string
}

func newStub(t *testing.T) *stub {
	t.Helper()
	s := &stub{
		operationStatus:  "succeeded",
		deliveryState:    "sent",
		conversationType: "rcs",
	}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)
	return s
}

func (s *stub) fail(code apierr.Code) { s.failure = &code }

func (s *stub) seen() []recorded {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]recorded(nil), s.requests...)
}

func (s *stub) countOf(method, path string) int {
	n := 0
	for _, r := range s.seen() {
		if r.Method == method && r.Path == path {
			n++
		}
	}
	return n
}

func (s *stub) serve(w http.ResponseWriter, r *http.Request) {
	body := readAll(r)
	s.mu.Lock()
	s.requests = append(s.requests, recorded{
		Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Body: body,
		Authorization: r.Header.Get("Authorization"), IdempotencyKey: r.Header.Get("Idempotency-Key"),
	})
	s.mu.Unlock()

	if s.refreshRejects && r.URL.Path == "/v1/auth/refresh" {
		writeError(w, apierr.CodeInvalidToken, "that refresh token is not one this server issued")
		return
	}
	if s.failure != nil {
		writeError(w, *s.failure, "the stub was told to answer "+string(*s.failure))
		return
	}

	// The attachment bytes are not an envelope.
	if strings.HasSuffix(r.URL.Path, "/content") && r.Method == http.MethodGet {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte("attachment bytes"))
		return
	}

	writeSuccess(w, s.dataFor(r))
}

func readAll(r *http.Request) string {
	if r.Body == nil {
		return ""
	}
	b, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	return string(b)
}

// dataFor answers each route with a body shaped like the DTO the spec
// describes. It is deliberately small: the CLI reads `operation`, `effect`,
// `items`, and the media and pairing fields, and nothing else.
func (s *stub) dataFor(r *http.Request) any {
	path := r.URL.Path
	base := strings.TrimPrefix(path, "/v1/")

	operation := map[string]any{
		"id": "op_01k4z2p8vv", "account_id": "acct_01k4z0aa", "kind": "send_text",
		"status": s.operationStatus, "terminal": s.operationStatus != "pending",
		"conversation_id": "conv_01k4z2p8vq", "message_id": "msg_01k4z2p8vt",
		"error": nil,
	}

	switch {
	case path == "/v1/pairing/start":
		return map[string]any{"pairing_id": "pair_01k4z2p8w0", "emoji": "🍎"}
	case strings.HasPrefix(path, "/v1/pairing/") && r.Method == http.MethodGet:
		return map[string]any{"state": "paired", "account_id": "acct_01k4z0aa", "emoji": "🍎"}
	case strings.HasPrefix(path, "/v1/pairing/") && r.Method == http.MethodDelete:
		return map[string]any{"state": "abandoned"}

	case path == "/v1/auth/admin-session":
		return map[string]any{
			"access_token": "agm_at_stub", "refresh_token": "agm_rt_stub",
			"scopes":     []string{"admin", "messages:read", "messages:write", "messages:delete"},
			"expires_at": "2026-09-06T10:11:07Z", "authorization_id": "auth_01k4z2p8w8",
		}
	case path == "/v1/auth/refresh":
		return map[string]any{"access_token": "agm_at_rotated", "refresh_token": "agm_rt_rotated"}
	case path == "/v1/auth/whoami":
		return map[string]any{"authorization_id": "auth_01k4z2p8w8", "kind": "admin",
			"scopes": []string{"admin"}}
	case path == "/v1/auth/logout":
		return map[string]any{"revoked": true}

	case path == "/v1/uploads" && r.Method == http.MethodPost:
		return map[string]any{
			"upload_id":  "upl_01k4z2p8wz",
			"upload_url": s.URL + "/v1/uploads/upl_01k4z2p8wz/content",
			"method":     "PUT",
			"token":      "agm_ut_stub",
			"limits":     map[string]any{"mime_type": "image/jpeg"},
		}
	case strings.HasPrefix(path, "/v1/uploads/") && r.Method == http.MethodPut:
		return map[string]any{"upload_id": "upl_01k4z2p8wz", "received": true}

	case strings.HasPrefix(path, "/v1/attachments/") && r.Method == http.MethodGet:
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/v1/attachments/"), "/content")
		return map[string]any{
			"attachment_id": id, "filename": "IMG_0421.jpg", "mime_type": "image/jpeg",
			"size": 16, "token": "agm_dt_stub",
			"download_url": s.URL + "/v1/attachments/" + id + "/content",
		}

	case strings.HasPrefix(path, "/v1/operations/") && r.Method == http.MethodGet:
		return operation

	case r.Method == http.MethodGet && strings.HasPrefix(path, "/v1/messages/") &&
		!strings.Contains(strings.TrimPrefix(path, "/v1/messages/"), "/"):
		return map[string]any{
			"id": "msg_01k4z2p8vt", "account_id": "acct_01k4z0aa",
			"conversation_id": "conv_01k4z2p8vq", "text": "on my way",
			"delivery_state": s.deliveryState, "direction": "outgoing",
		}

	case strings.HasPrefix(path, "/v1/conversations/") && r.Method == http.MethodGet &&
		!strings.Contains(strings.TrimPrefix(path, "/v1/conversations/"), "/"):
		return map[string]any{
			"id": "conv_01k4z2p8vq", "account_id": "acct_01k4z0aa", "name": "Alex",
			"type": s.conversationType, "unread": false,
		}

	case strings.HasSuffix(path, "/context"):
		return map[string]any{"items": []any{}}

	case r.Method == http.MethodDelete && strings.HasPrefix(path, "/v1/messages/") &&
		!strings.Contains(strings.TrimPrefix(path, "/v1/messages/"), "/"):
		return map[string]any{"operation": operation, "effect": s.effectOr(apierr.EffectMessageDelete)}
	case r.Method == http.MethodDelete && strings.HasPrefix(path, "/v1/conversations/") &&
		!strings.Contains(strings.TrimPrefix(path, "/v1/conversations/"), "/"):
		return map[string]any{"operation": operation, "effect": s.effectOr(apierr.EffectConversationDelete)}
	case r.Method == http.MethodDelete && strings.HasPrefix(path, "/v1/accounts/"):
		return map[string]any{"deleted": map[string]any{"messages": 0},
			"effect": s.effectOr(apierr.EffectAccountRemove)}

	case path == "/v1/health":
		return map[string]any{"status": "ok", "version": "stub"}
	case path == "/v1/admin/settings" && r.Method == http.MethodGet:
		return map[string]any{"items": []any{
			map[string]any{"key": "operations.wait_timeout", "value": "60s", "source": "default"},
		}}
	case strings.HasPrefix(path, "/v1/admin/settings/"):
		return map[string]any{"key": strings.TrimPrefix(path, "/v1/admin/settings/"),
			"value": "60s", "source": "default"}
	case path == "/v1/admin/settings" && r.Method == http.MethodPatch:
		return map[string]any{"changed": []string{"operations.wait_timeout"}}
	case path == "/v1/admin/backup":
		return map[string]any{"path": "backups/agent-gm-stub.sqlite3", "bytes": 4096}
	case path == "/v1/admin/backfill":
		return map[string]any{"operation": operation, "accounts": 1}
	case path == "/v1/admin/diagnostics":
		return map[string]any{"account_id": "acct_01k4z0aa", "dropped_events": 0}

	case base == "accounts" && r.Method == http.MethodGet:
		return map[string]any{"items": []any{
			map[string]any{"id": "acct_01k4z0aa", "google_account": "owner@example.test",
				"label": "personal", "state": "connected"},
		}}
	case strings.HasPrefix(path, "/v1/accounts/") && r.Method == http.MethodGet:
		return map[string]any{"id": "acct_01k4z0aa", "google_account": "owner@example.test",
			"state": "connected"}

	case r.Method == http.MethodGet:
		// Every remaining GET is a listing (spec section 7.1: a listing puts
		// its rows in data.items).
		return map[string]any{"items": []any{
			map[string]any{"id": "conv_01k4z2p8vq", "account_id": "acct_01k4z0aa", "name": "Alex"},
		}}
	default:
		return map[string]any{"operation": operation, "changed": true}
	}
}

func (s *stub) effectOr(fallback string) string {
	if s.effect != "" {
		return s.effect
	}
	return fallback
}

func writeSuccess(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	// Every answer carries a warning, because a warning is the server's
	// channel for saying something (spec section 7.1) and it belongs on
	// stderr: a run with nothing to say would not prove the two streams are
	// separated.
	_ = json.NewEncoder(w).Encode(map[string]any{
		"data": data, "next_cursor": nil, "warnings": []string{"history_incomplete"},
		"request_id": "req_01k4z2p8stub",
	})
}

func writeError(w http.ResponseWriter, code apierr.Code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(apierr.HTTPStatus(code))
	retryable := apierr.RetryabilityOf(code) == apierr.RetryYes
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"code": string(code), "message": message, "retryable": retryable,
			"details": map[string]any{"operation_id": "op_01k4z2p8vv"},
		},
		"request_id": "req_01k4z2p8stub",
	})
}

// result is one `agm` invocation's whole output, captured separately so a
// test can assert what is on stdout and what is on stderr -- which is the
// entire content of section 16 Slice 2 test 26.
type result struct {
	code   int
	stdout string
	stderr string
}

// runCLI drives the real command line: real parsing, real dispatch, real
// client, against the stub. Nothing is called directly.
func runCLI(t *testing.T, s *stub, env map[string]string, stdin string, args ...string) result {
	t.Helper()

	stateDir := t.TempDir()
	base := map[string]string{
		"AGENT_GM_ACCESS_TOKEN":     "agm_at_test",
		"AGENT_GM_URL":              s.URL,
		"AGENT_GM_CREDENTIALS_FILE": filepath.Join(stateDir, "credentials.json"),
	}
	for k, v := range env {
		if v == "" {
			delete(base, k)
			continue
		}
		base[k] = v
	}

	var stdout, stderr strings.Builder
	code := cli.Run(cli.Env{
		Args:         args,
		Stdin:        strings.NewReader(stdin),
		Stdout:       &stdout,
		Stderr:       &stderr,
		Getenv:       func(k string) string { return base[k] },
		HTTP:         s.Client(),
		Version:      "test",
		PollInterval: time.Millisecond,
	})
	return result{code: code, stdout: stdout.String(), stderr: stderr.String()}
}

// pasteFixture writes a cookie paste with fictional values. No real cookie,
// no real number, nothing from a session file (spec section 12.1).
func pasteFixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "paste.json")
	cookies := map[string]string{
		"SID": "fixture-sid", "HSID": "fixture-hsid", "OSID": "fixture-osid",
		"SSID": "fixture-ssid", "APISID": "fixture-apisid", "SAPISID": "fixture-sapisid",
	}
	raw, err := json.Marshal(cookies)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
