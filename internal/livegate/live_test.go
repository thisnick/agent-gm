//go:build live

// Package livegate holds the Slice 1 live gate of spec section 16.
//
// These tests are `go test -tags live` plus AGENT_GM_LIVE=1, run by
// `devbox run test-live`. They are not in CI. They are run BY THE COORDINATOR
// ONLY, never by an implementer or a reviewer, from a checkout pinned to the
// reviewer-accepted commit -- never from an implementer's working tree,
// because a mid-edit tree failing to build is what makes a live gate
// meaningless (spec section 13.3).
//
// Live sends go ONLY to numbers the owner has approved, referred to
// throughout this repository by placeholder. The real values never enter this
// repository: they arrive at run time through AGENT_GM_LIVE_NUMBERS or the
// untracked, git-ignored testdata/live-numbers.local.
package livegate

import (
	"bufio"
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/thisnick/agent-gm/internal/accounts"
	"github.com/thisnick/agent-gm/internal/clock"
	"github.com/thisnick/agent-gm/internal/config"
	"github.com/thisnick/agent-gm/internal/gm"
	"github.com/thisnick/agent-gm/internal/store"
)

// LiveNumbers are the approved targets, in the order spec section 13.3 names
// them: <APPROVED_DIRECT_NUMBER>, <APPROVED_GROUP_NUMBER_1>,
// <APPROVED_GROUP_NUMBER_2>.
type LiveNumbers struct {
	Direct string
	Group1 string
	Group2 string
}

// requireGate skips unless the gate is open. AGENT_GM_LIVE=1 is the variable
// the spec names, and the `live` build tag is the other half.
func requireGate(t *testing.T) {
	t.Helper()
	if os.Getenv("AGENT_GM_LIVE") != "1" {
		t.Skip("the live gate is closed: set AGENT_GM_LIVE=1 and build with -tags live")
	}
}

// approvedNumbers reads AGENT_GM_LIVE_NUMBERS ("direct,group1,group2"), or
// the untracked testdata/live-numbers.local. It never defaults to anything:
// a live send with no approved target does not happen.
func approvedNumbers(t *testing.T) LiveNumbers {
	t.Helper()
	raw := os.Getenv("AGENT_GM_LIVE_NUMBERS")
	if raw == "" {
		f, err := os.Open("../../testdata/live-numbers.local")
		if err != nil {
			t.Skip("no approved numbers: set AGENT_GM_LIVE_NUMBERS, or create the " +
				"untracked testdata/live-numbers.local")
		}
		defer func() { _ = f.Close() }()
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			raw = line
			break
		}
	}
	parts := strings.Split(raw, ",")
	if len(parts) < 1 || strings.TrimSpace(parts[0]) == "" {
		t.Fatal("AGENT_GM_LIVE_NUMBERS must be direct[,group1,group2]")
	}
	n := LiveNumbers{Direct: strings.TrimSpace(parts[0])}
	if len(parts) > 1 {
		n.Group1 = strings.TrimSpace(parts[1])
	}
	if len(parts) > 2 {
		n.Group2 = strings.TrimSpace(parts[2])
	}
	return n
}

type liveEnv struct {
	store    *store.Store
	sessions *store.SessionStore
	sup      *accounts.Supervisor
	accounts []*accounts.Account
}

// openLive attaches to the coordinator's real, already-paired deployment. It
// never pairs: pairing is live-gate test 7, which the coordinator drives by
// hand with the owner at the phone.
func openLive(t *testing.T) *liveEnv {
	t.Helper()
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("loading the environment: %v", err)
	}
	if cfg.Backend != config.BackendLibGM {
		t.Fatalf("the live gate runs against the real backend, not %q", cfg.Backend)
	}
	st, err := store.Open(cfg.DataDir, clock.Real{})
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	sessions, err := store.NewSessionStore(cfg.DataDir, cfg.DataKey)
	if err != nil {
		t.Fatal(err)
	}
	sup := accounts.New(st, sessions, clock.Real{}, nil)
	sup.ManualIngest = true

	ctx := context.Background()
	rows, err := st.Accounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	env := &liveEnv{store: st, sessions: sessions, sup: sup}
	for _, row := range rows {
		if !row.SessionPresent {
			continue
		}
		blob, err := sessions.Load(row.ID)
		if err != nil {
			t.Fatalf("%s: %v", row.ID, err)
		}
		backend, err := gm.NewFromSession(blob, zeroLogger())
		if err != nil {
			t.Fatalf("%s: %v", row.ID, err)
		}
		a := sup.Adopt(ctx, row.ID, row.GoogleAccount, backend)
		if err := sup.Start(ctx, a); err != nil {
			t.Fatalf("%s could not connect: %v", row.ID, err)
		}
		env.accounts = append(env.accounts, a)
	}
	t.Cleanup(func() { sup.StopAll(context.Background()) })
	if len(env.accounts) == 0 {
		t.Skip("no paired account in this data directory: run `agent-gm spike pair` first")
	}
	return env
}

// Live gate test 9: `agent-gm spike list` returns the owner's real
// conversation list, and the row for <APPROVED_DIRECT_NUMBER> is present with
// a conv_ ID.
func TestLiveListFindsTheApprovedDirectConversation(t *testing.T) {
	requireGate(t)
	numbers := approvedNumbers(t)
	env := openLive(t)
	ctx := context.Background()
	a := env.accounts[0]

	convs, err := a.Backend.ListConversations(ctx, gm.FolderInbox, 200)
	if err != nil {
		t.Fatalf("ListConversations: %v", err)
	}
	if len(convs) == 0 {
		t.Fatal("the account has no conversations")
	}
	var found string
	for _, c := range convs {
		id, err := a.Ingester().IngestConversation(ctx, c)
		if err != nil {
			t.Fatalf("ingesting: %v", err)
		}
		for _, p := range c.Participants {
			if p.IsMe {
				continue
			}
			if sameNumber(p.PhoneE164, numbers.Direct) {
				found = id
			}
		}
	}
	if found == "" {
		t.Fatal("the approved direct conversation is not in the list")
	}
	if !store.HasPrefix(found, store.PrefixConversation) {
		t.Errorf("the conversation ID %q is not a conv_ ID", found)
	}
	t.Logf("approved direct conversation: %s", found)
}

// Live gate test 10: a send to <APPROVED_DIRECT_NUMBER> AND NO OTHER NUMBER
// returns SUCCESS, and the remote echo carries back the same bare-UUID TmpID
// we sent, followed by at least one delivery-status update.
//
// The send itself is the coordinator's act. This test refuses to run unless
// AGENT_GM_LIVE_SEND=1 is set as well, so opening the gate for the read-only
// checks cannot post a message to a real person by accident.
func TestLiveSendToTheApprovedDirectNumber(t *testing.T) {
	requireGate(t)
	if os.Getenv("AGENT_GM_LIVE_SEND") != "1" {
		t.Skip("this test sends a real message: set AGENT_GM_LIVE_SEND=1 to allow it")
	}
	numbers := approvedNumbers(t)
	env := openLive(t)
	ctx := context.Background()
	a := env.accounts[0]

	conv, err := findApprovedConversation(ctx, a, numbers.Direct)
	if err != nil {
		t.Fatalf("finding the approved conversation: %v", err)
	}
	// The target is never in question: this is the conversation whose only
	// other participant is the approved direct number.
	for _, p := range conv.Participants {
		if p.IsMe || p.PhoneE164 == "" {
			continue
		}
		if !sameNumber(p.PhoneE164, numbers.Direct) {
			t.Fatalf("the conversation has a participant that is not the approved number; refusing to send")
		}
	}

	tmpID := gm.GenerateTmpID()
	res, err := a.Backend.SendText(ctx, gm.SendTextRequest{
		ConversationID: conv.SourceID,
		ParticipantID:  conv.DefaultOutgoingID,
		Text:           "agent-gm slice 1 test",
		TmpID:          tmpID,
		SIMPayload:     conv.SIMPayload,
	})
	if err != nil {
		t.Fatalf("SendText: %v", err)
	}
	if res.Status != gm.SendStatusSuccess {
		t.Fatalf("send status = %s, want SUCCESS", res.Status)
	}

	// The echo carries back the same bare UUID, then at least one
	// delivery-status update.
	echo := waitForEcho(t, ctx, a, tmpID, 60*time.Second)
	if echo.TmpID != tmpID {
		t.Fatalf("the echo carried tmp_id %q, want %q", echo.TmpID, tmpID)
	}
	t.Logf("echo: %s state=%s", echo.SourceID, echo.DeliveryState)

	states := map[gm.DeliveryState]bool{echo.DeliveryState: true}
	deadline := time.Now().Add(120 * time.Second)
	for time.Now().Before(deadline) && len(states) < 2 {
		select {
		case ev := <-a.Backend.Events():
			a.Apply(ctx, ev)
			if m, ok := ev.(*gm.EventMessage); ok && m.Message.TmpID == tmpID {
				states[m.Message.DeliveryState] = true
			}
		case <-time.After(time.Second):
		}
	}
	if len(states) < 2 {
		t.Fatalf("no delivery-status update arrived; saw only %v", states)
	}
	t.Logf("delivery states observed: %v", states)
}

// Live gate test 11: the owner replies from that phone and the inbound
// message appears within 10 seconds, with the right text and sender.
func TestLiveInboundReplyArrives(t *testing.T) {
	requireGate(t)
	if os.Getenv("AGENT_GM_LIVE_EXPECT_REPLY") == "" {
		t.Skip("set AGENT_GM_LIVE_EXPECT_REPLY to the text the owner is about to send")
	}
	want := os.Getenv("AGENT_GM_LIVE_EXPECT_REPLY")
	numbers := approvedNumbers(t)
	env := openLive(t)
	ctx := context.Background()
	a := env.accounts[0]

	t.Logf("waiting for the owner to reply with %q from the approved phone", want)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case ev := <-a.Backend.Events():
			a.Apply(ctx, ev)
			m, ok := ev.(*gm.EventMessage)
			if !ok || m.IsOld {
				continue
			}
			if m.Message.Direction() != gm.DirectionIncoming {
				continue
			}
			if strings.TrimSpace(m.Message.Text) != strings.TrimSpace(want) {
				continue
			}
			t.Logf("inbound message %s from %s", m.Message.SourceID, m.Message.ParticipantID)
			return
		case <-time.After(200 * time.Millisecond):
		}
	}
	t.Fatalf("no inbound reply within 10 seconds (approved sender %s)", redact(numbers.Direct))
}

// Live gate test 12: restart the process; each account's session file
// reloads, Connect succeeds without re-pairing, and IsLoggedIn is true.
func TestLiveSessionReloadsWithoutRePairing(t *testing.T) {
	requireGate(t)
	env := openLive(t)
	for _, a := range env.accounts {
		if !a.Backend.IsLoggedIn() {
			t.Errorf("%s is not logged in after reloading its session", a.ID)
		}
		row, err := env.store.Account(context.Background(), a.ID)
		if err != nil {
			t.Fatal(err)
		}
		if row.State != store.StateConnected {
			t.Errorf("%s is %s after a restart, want connected", a.ID, row.State)
		}
		if !row.SessionPresent {
			t.Errorf("%s has no session file", a.ID)
		}
	}
}

// Live gate test 13: diag prints the compiled and live ConfigVersion and
// is_default_sms_app from a real round trip. The live values are the point;
// the fake covers the shape.
func TestLiveConfigVersionsAndDefaultSMSApp(t *testing.T) {
	requireGate(t)
	env := openLive(t)
	ctx := context.Background()
	compiled := gm.CompiledConfigVersion()
	for _, a := range env.accounts {
		info, err := a.Backend.FetchConfig(ctx)
		if err != nil {
			t.Fatalf("%s: FetchConfig: %v", a.ID, err)
		}
		isDefault, err := a.Backend.IsDefaultSMSApp(ctx)
		if err != nil {
			t.Fatalf("%s: IsDefaultSMSApp: %v", a.ID, err)
		}
		// Recorded in the slice report.
		t.Logf("%s config_version_compiled=%s config_version_live=%s stale=%v is_default_sms_app=%v",
			a.ID, compiled, info.Live, !compiled.SameDate(info.Live), isDefault)
		if info.Live == (gm.ConfigVersion{}) {
			t.Errorf("%s: the live ConfigVersion is empty", a.ID)
		}
		if !isDefault {
			t.Errorf("%s: Google Messages is not the phone's default SMS app, so sends will fail", a.ID)
		}
	}
}

// --- helpers -----------------------------------------------------------------

func findApprovedConversation(ctx context.Context, a *accounts.Account, number string) (gm.Conversation, error) {
	convs, err := a.Backend.ListConversations(ctx, gm.FolderInbox, 200)
	if err != nil {
		return gm.Conversation{}, err
	}
	for _, c := range convs {
		if c.IsGroup {
			continue
		}
		for _, p := range c.Participants {
			if !p.IsMe && sameNumber(p.PhoneE164, number) {
				return c, nil
			}
		}
	}
	return gm.Conversation{}, errors.New("no direct conversation with the approved number")
}

func waitForEcho(t *testing.T, ctx context.Context, a *accounts.Account, tmpID string, within time.Duration) gm.Message {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		select {
		case ev := <-a.Backend.Events():
			a.Apply(ctx, ev)
			if m, ok := ev.(*gm.EventMessage); ok && m.Message.TmpID == tmpID {
				return m.Message
			}
		case <-time.After(200 * time.Millisecond):
		}
	}
	t.Fatalf("no echo carrying tmp_id %s within %s", tmpID, within)
	return gm.Message{}
}

// sameNumber compares two numbers by their digits, so formatting differences
// do not matter.
func sameNumber(a, b string) bool {
	return digits(a) != "" && digits(a) == digits(b)
}

func digits(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// redact renders a number the way a log line would: never the number itself.
func redact(s string) string {
	d := digits(s)
	if len(d) < 4 {
		return "<approved number>"
	}
	return "<approved number ending " + d[len(d)-4:] + ">"
}

// zeroLogger gives libgm a logger at info: at trace it base64-logs decrypted
// payloads, which the live gate must not do (spec section 12.2).
func zeroLogger() zerolog.Logger {
	return zerolog.New(os.Stderr).Level(zerolog.InfoLevel).With().Timestamp().Logger()
}
