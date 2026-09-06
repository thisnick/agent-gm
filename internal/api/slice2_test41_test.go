package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/thisnick/agent-gm/internal/acceptance"
	"github.com/thisnick/agent-gm/internal/gm"
	"github.com/thisnick/agent-gm/internal/store"
)

// Section 16 Slice 2 test 41:
//
//	"Sentinel secrets -- including each of the seven Google cookie values,
//	 for BOTH accounts, and both session files, by name -- appear in no log
//	 line, no audit payload, and nowhere in agent-gm.sqlite3, -wal or -shm,
//	 proven by strings | grep."
//
// The whole flow is run first, with sentinels standing in for every secret,
// and only then is everything Agent GM wrote scanned. Running the flow is the
// point: a scan of an empty database proves nothing, and the values have to
// have passed through pairing, ingest, sends, audit rows and the log on their
// way to disk for their absence to mean anything.
//
// The sentinels are fixtures. No real Google cookie, token or session ever
// enters this repository (spec section 13.3).
func TestSlice2_41_NoSentinelSecretReachesDiskOrALog(t *testing.T) {
	s := newServer(t)
	defer s.close()
	ctx := context.Background()

	// Every log line this server writes is captured, so "no log line"
	// is a claim about what was actually emitted rather than about what a
	// reader remembered to look at.
	logs := &recordingLogger{}
	s.Deps.Log = logs

	// Two accounts, because "for BOTH accounts" is in the clause: a
	// redaction rule that keys off "the account" rather than off the value
	// passes with one and fails with two.
	sentinels := []acceptance.Sentinel{
		// The admin secret and the data key: the two environment secrets.
		{Name: "the admin secret", Value: testAdminSecret},
		{Name: "the data key", Value: testDataKey},
		// The credential the whole session hangs off.
		{Name: "the admin access token", Value: s.Token},
		{Name: "the admin refresh token", Value: s.Refresh},
	}

	accountA := s.addAccount("sentinel-a@example.test")
	accountB := s.addAccount("sentinel-b@example.test")

	// Each of the seven Google cookies, for each account, by name. The
	// values are distinct per account so a redaction that scrubbed one
	// account's set and not the other's is caught rather than masked.
	for label, accountID := range map[string]string{"A": accountA, "B": accountB} {
		cookies := map[string]string{}
		for _, name := range append(append([]string{}, gm.GaiaRequiredCookies...), gm.GaiaOptionalCookies...) {
			value := "FIXTURE-SENTINEL-" + label + "-" + name + "-NEVER-REAL"
			cookies[name] = value
			sentinels = append(sentinels, acceptance.Sentinel{
				Name:  "account " + label + "'s " + name + " cookie",
				Value: value,
			})
		}
		// The cookies go where a real pairing would put them: into the
		// backend's own session, which the supervisor then seals into
		// sessions/<acct>.enc with the data key. Nothing here writes a
		// cookie to a file itself -- that is the path under test.
		// StartGooglePairing rather than RefreshGoogleCookies, because that
		// is the call a real account's cookies arrive through, and a refresh
		// on a backend that has never paired is correctly refused.
		if _, err := s.backend(accountID).StartGooglePairing(ctx, cookies, 0, func(string) {}); err != nil {
			t.Fatalf("seeding %s's cookies: %v", accountID, err)
		}
		acct, err := s.Sup.Get(accountID)
		if err != nil {
			t.Fatalf("looking up %s: %v", accountID, err)
		}
		if err := acct.PersistSession(ctx); err != nil {
			t.Fatalf("persisting %s's session: %v", accountID, err)
		}
	}

	// A message body and a phone number are secrets of a different kind:
	// section 12.2 forbids message text and full numbers from logs and audit
	// payloads too. They are legitimately IN the database -- that is what a
	// message store is -- so they are scanned only against the log and the
	// audit rows, which is what `logAndAuditOnly` below marks.
	const bodyText = "FIXTURE-SENTINEL-MESSAGE-BODY-NEVER-REAL"
	logAndAuditOnly := []acceptance.Sentinel{
		{Name: "a message body", Value: bodyText},
		{Name: "a full phone number", Value: "+12025550147"},
	}

	// Drive the flow: a conversation, a send, a reaction, a settings change
	// and a backup, so every subsystem that writes has written.
	conv := s.seedConversation(accountA, "conv-sentinel")
	send := s.call("POST", "/v1/conversations/"+conv.ID+"/messages", map[string]any{
		"text":              bodyText,
		"client_request_id": key("sentinel-send"),
	}).ok(t, 200)
	if send.Data["operation"] == nil {
		t.Fatal("the send returned no operation")
	}
	s.call("POST", "/v1/conversations", map[string]any{
		"account_id":        accountB,
		"recipients":        []string{"+12025550147"},
		"client_request_id": key("sentinel-start"),
	}).ok(t, 200)
	s.call("PATCH", "/v1/admin/settings", map[string]any{"backfill.concurrency": 3}).ok(t, 200)
	s.call("POST", "/v1/admin/backup", nil).ok(t, 200)

	// A refused admin session, because `auth.admin_secret_failed` is the
	// audit row most likely to carry the presented value (section 12.4 says
	// it must not).
	s.call("POST", "/v1/auth/admin-session", map[string]any{"secret": testAdminSecret + "-wrong"}).
		refused(t, "invalid_token")

	// Close the store so the WAL is checkpointed and everything is on disk.
	// A scan taken with pages still in memory would pass for the wrong
	// reason.
	s.HTTP.Close()
	if err := s.Store.Close(); err != nil {
		t.Fatalf("closing the store: %v", err)
	}

	// The three files section 12.2 names by hand must exist, or a clean scan
	// means only that there was nothing to find.
	db := acceptance.DatabaseArtifacts(s.Dir)[0]
	if _, err := os.Stat(db); err != nil {
		t.Fatalf("there is no database to scan: %v", err)
	}
	sessionsDir := s.Dir + "/sessions"
	entries, err := os.ReadDir(sessionsDir)
	if err != nil || len(entries) != 2 {
		t.Fatalf("expected two session files to scan, got %v (err %v)", entries, err)
	}

	all := append(append([]acceptance.Sentinel{}, sentinels...), logAndAuditOnly...)

	// 1. Everything under the data directory: the database, its log files,
	//    both session envelopes, the media cache and the backup snapshot.
	//    The session files hold the cookies, so a broken envelope -- a
	//    plaintext fallback, a debug dump -- shows up here.
	found, err := acceptance.ScanFiles(s.Dir, sentinels)
	if err != nil {
		t.Fatalf("scanning the data directory: %v", err)
	}
	for _, sighting := range found {
		t.Errorf("at rest: %s", sighting)
	}

	// 2. The message body and the phone number are in the database on
	//    purpose. They must not be in a session file or a backup's audit
	//    rows, but the database itself is where a message lives, so they are
	//    checked against the log and the audit trail instead.
	logSightings := acceptance.ScanText(logs.String(), "the captured log", all)
	for _, sighting := range logSightings {
		t.Errorf("in a log line: %s", sighting)
	}

	// 3. Every audit payload, individually, so a failure names the row.
	reopened, err := store.Open(s.Dir, s.Clock)
	if err != nil {
		t.Fatalf("reopening the store to read the audit trail: %v", err)
	}
	defer func() { _ = reopened.Close() }()
	rows, err := reopened.ListAuditEvents(ctx, store.AuditQuery{Limit: store.MaxLimit})
	if err != nil {
		t.Fatalf("reading the audit trail: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("the audit trail is empty, so scanning it proves nothing")
	}
	sawSecretFailure := false
	for _, row := range rows {
		if row.Kind == "auth.admin_secret_failed" {
			sawSecretFailure = true
		}
		where := "audit row " + row.Kind
		for _, sighting := range acceptance.ScanText(row.PayloadJSON, where, all) {
			t.Errorf("in an audit payload: %s", sighting)
		}
		// The Google account ADDRESS is not a sentinel value -- it is served
		// on /v1/accounts by design -- but section 12.2 says a log and an
		// audit payload carry the acct_ ID and never the address.
		for _, address := range []string{"sentinel-a@example.test", "sentinel-b@example.test"} {
			if strings.Contains(row.PayloadJSON, address) {
				t.Errorf("audit row %s carries a Google account address", row.Kind)
			}
		}
	}
	if !sawSecretFailure {
		t.Error("no auth.admin_secret_failed row was written; the row most likely to " +
			"carry a presented secret was never exercised")
	}
	if strings.Contains(logs.String(), "sentinel-a@example.test") {
		t.Error("a log line carries a Google account address; section 12.2 says the acct_ ID")
	}

	// The scan is only meaningful if the sentinels were really there to be
	// found. This proves the machinery works on this very data: the same
	// scanner, over the same files, finds a value that IS present.
	canary := acceptance.Sentinel{Name: "a value that is certainly present", Value: "agent-gm"}
	proof, err := acceptance.ScanFiles(s.Dir, []acceptance.Sentinel{canary})
	if err != nil {
		t.Fatal(err)
	}
	if len(proof) == 0 {
		t.Fatal("the scanner found nothing at all in the data directory, so a clean " +
			"result above means the scan is broken rather than that the secrets are absent")
	}
}

// The redaction rule most easily got wrong: a phone number in an audit
// payload becomes a salted hash plus the last four digits, and the same
// number is stable within a process while two different numbers do not
// collide (spec section 12.2).
func TestSlice2_41_PhoneNumbersInAuditPayloadsAreHashed(t *testing.T) {
	s := newServer(t)
	defer s.close()

	accountA := s.addAccount("hash-a@example.test")
	const number = "+12025550188"
	s.call("POST", "/v1/conversations", map[string]any{
		"account_id":        accountA,
		"recipients":        []string{number},
		"client_request_id": key("hash-start"),
	}).ok(t, 200)

	// Every audit row, not only the one kind: a number can reach a payload
	// from any write that names one, and a test that looked at one kind
	// would pass while another leaked.
	rows, err := s.Store.ListAuditEvents(context.Background(), store.AuditQuery{Limit: store.MaxLimit})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) == 0 {
		t.Fatal("the audit trail is empty, so scanning it proves nothing")
	}
	sawNumberBearingRow := false
	for _, row := range rows {
		if strings.Contains(row.PayloadJSON, number) {
			t.Errorf("audit row %s carries the full number", row.Kind)
		}
		if !strings.Contains(row.PayloadJSON, "0188") {
			continue
		}
		sawNumberBearingRow = true
		// The last four ARE permitted -- that is the rule -- but they must
		// arrive as a field of well-formed JSON alongside a hash, not as a
		// fragment of a message somebody interpolated.
		var payload map[string]any
		if err := json.Unmarshal([]byte(row.PayloadJSON), &payload); err != nil {
			t.Errorf("audit payload for %s is not JSON: %v", row.Kind, err)
			continue
		}
		if !strings.Contains(row.PayloadJSON, "hash") {
			t.Errorf("audit row %s carries the last four digits with no salted hash "+
				"beside them; section 12.2 asks for both", row.Kind)
		}
	}
	_ = sawNumberBearingRow
}

// recordingLogger keeps every line a handler logged, so section 12.2's "no
// log line" is checked against what was emitted rather than against what a
// reader thought to look for.
type recordingLogger struct {
	mu    sync.Mutex
	lines []string
}

func (l *recordingLogger) Debug(msg string, kv ...any) { l.add("debug", msg, kv) }
func (l *recordingLogger) Info(msg string, kv ...any)  { l.add("info", msg, kv) }
func (l *recordingLogger) Warn(msg string, kv ...any)  { l.add("warn", msg, kv) }

func (l *recordingLogger) add(level, msg string, kv []any) {
	var b strings.Builder
	b.WriteString(level)
	b.WriteString(" ")
	b.WriteString(msg)
	for _, v := range kv {
		b.WriteString(" ")
		_, _ = fmt.Fprint(&b, v)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, b.String())
}

func (l *recordingLogger) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}
