package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/thisnick/agent-gm/internal/accounts"
	"github.com/thisnick/agent-gm/internal/api"
	"github.com/thisnick/agent-gm/internal/audit"
	"github.com/thisnick/agent-gm/internal/authz"
	"github.com/thisnick/agent-gm/internal/clock"
	"github.com/thisnick/agent-gm/internal/core"
	"github.com/thisnick/agent-gm/internal/gm"
	"github.com/thisnick/agent-gm/internal/gm/fake"
	"github.com/thisnick/agent-gm/internal/media"
	"github.com/thisnick/agent-gm/internal/settings"
	"github.com/thisnick/agent-gm/internal/store"
)

// The harness for the Slice 2 acceptance tests this package owns: 1, 13, 15,
// 16, 17, 18, 19, 20, 33 and 43.
//
// Nothing here needs a phone, a network or Docker, so none of these tests is
// behind a gate. Parking a test behind a gate it does not need is how a
// clause stays unverified for a slice.
//
// **ONE FAKE IS ONE ACCOUNT** (spec section 13.1). A test that wants two
// accounts registers two fakes with one supervisor over one store, which is
// exactly the shape production has: the fake never models several accounts
// internally, so the multi-account paths above `gm` are exercised against the
// real arrangement rather than a convenient one.
//
// **Fictional numbers only.** `+1 202 555 01xx` is reserved for fiction. No
// real number appears anywhere in this repository, in a test, a fixture, a
// comment or a commit message.
const (
	fictionalA = "+12025550101"
	fictionalB = "+12025550102"

	addressA = "owner-a@example.test"
	addressB = "owner-b@example.test"

	// testDataKey is 64 hex characters and is not a secret: it exists only
	// inside a t.TempDir() that is deleted when the test ends.
	testDataKey = "0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20"

	// testAdminSecret is well over the 43-byte minimum spec section 12.1
	// puts on AGENT_GM_ADMIN_SECRET.
	testAdminSecret = "test-admin-secret-not-a-real-one-0123456789abcdefghijklmnop"

	// publicURL is what every URL the server hands out must be built from,
	// whatever a request's Host says (spec sections 10.3, 12.3).
	publicURL = "https://gm.example.test"
)

// server is a whole fake-backed Agent GM, in process, over httptest.
type server struct {
	t     *testing.T
	Dir   string
	Clock *clock.Fake
	Store *store.Store
	Deps  *api.HandlerDeps
	Sup   *accounts.Supervisor
	HTTP  *httptest.Server

	// Token is an admin session carrying admin plus the three messaging
	// scopes, which is what spec section 9.7 says the bootstrap mints.
	Token string
	// AuthorizationID is that session's authorization.
	AuthorizationID string
	// Refresh is the session's refresh token. A refresh is the operation
	// that NARROWS an existing authorization (spec section 9.6); a second
	// mint would create a new one, which is a different fact.
	Refresh string

	accounts map[string]*fake.Backend
}

// newServer builds a fresh one in a new temporary directory.
func newServer(t *testing.T) *server {
	t.Helper()
	return openServer(t, t.TempDir(), clock.NewFake())
}

// openServer builds a server over an EXISTING directory, which is how a
// restart is modelled: the same files, a new process's worth of state.
func openServer(t *testing.T, dir string, clk *clock.Fake) *server {
	t.Helper()
	ctx := context.Background()

	st, err := store.Open(dir, clk)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	key, err := store.ParseDataKey(testDataKey)
	if err != nil {
		t.Fatalf("parsing the data key: %v", err)
	}
	sessions, err := store.NewSessionStore(dir, key)
	if err != nil {
		t.Fatalf("opening the session store: %v", err)
	}
	signer, err := media.NewSigner(key)
	if err != nil {
		t.Fatalf("building the ticket signer: %v", err)
	}

	// The audit rows go into `audit_events` for real, not into memory: test 33
	// asserts an account.removed row SURVIVES the erasure of the account it
	// names, which a memory appender would pass for the wrong reason.
	appender := api.NewStoreAppender(st)
	sup := accounts.New(st, sessions, clk, nil)

	authzService, err := authz.New(st, clk, authz.NewMemorySettings(), authz.NewRecordingAudit(), authz.Config{
		AdminSecret: testAdminSecret,
		PublicURL:   publicURL,
	})
	if err != nil {
		t.Fatalf("building the credential service: %v", err)
	}

	deps := &api.HandlerDeps{
		Store:                 st,
		Sessions:              sessions,
		Supervisor:            sup,
		Authz:                 authzService,
		Settings:              settings.New(settings.NewRegistry(), settings.NewMemory(), func(string) string { return "" }),
		Audit:                 audit.NewWriter(appender, clk),
		Signer:                signer,
		Cache:                 &media.Cache{Dir: dir + "/media-cache", Index: media.NewStoreIndex(st), MaxBytes: 2 << 30},
		Clock:                 clk,
		DataKey:               key,
		DataDir:               dir,
		PublicURL:             publicURL,
		Version:               "0.0.0-test",
		Commit:                "abc1234",
		SourceURLBase:         "https://github.com/thisnick/agent-gm",
		ConfigVersionCompiled: gm.CompiledConfigVersion().String(),
		UpstreamCommit:        gm.PinnedUpstreamCommit,
		Core:                  core.DefaultConfig(),
		Heartbeat:             50 * time.Millisecond,
	}

	apiServer := api.NewServer(api.Deps{Authz: authzService, PublicURL: publicURL})
	if err := api.RegisterAll(apiServer, deps); err != nil {
		t.Fatalf("registering handlers: %v", err)
	}
	httpServer := httptest.NewServer(apiServer)
	t.Cleanup(httpServer.Close)

	s := &server{
		t: t, Dir: dir, Clock: clk, Store: st, Deps: deps, Sup: sup,
		HTTP: httpServer, accounts: map[string]*fake.Backend{},
	}

	// Re-adopt any account whose row survived the "restart", so a second
	// server over the same directory has the same supervised accounts.
	rows, err := st.Accounts(ctx)
	if err != nil {
		t.Fatalf("listing accounts: %v", err)
	}
	for _, row := range rows {
		be := fake.New(row.GoogleAccount, fake.WithClock(clk))
		sup.Adopt(ctx, row.ID, row.GoogleAccount, be)
		s.accounts[row.ID] = be
	}

	sess, err := authzService.MintAdminSession(ctx, testAdminSecret, nil, "127.0.0.1")
	if err != nil {
		t.Fatalf("minting the admin session: %v", err)
	}
	s.Token = sess.AccessToken
	s.Refresh = sess.RefreshToken
	s.AuthorizationID = sess.AuthorizationID
	return s
}

// addAccount registers one more account: one fake, one row, one supervised
// backend. Calling it twice is how a two-account test is built.
func (s *server) addAccount(address string) string {
	s.t.Helper()
	ctx := context.Background()
	id := store.AccountID(address)
	if err := s.Store.UpsertAccount(ctx, store.Account{
		ID:             id,
		GoogleAccount:  address,
		State:          store.StateConnected,
		SessionPresent: true,
		PairedAtMS:     s.Clock.Now().UnixMilli(),
	}); err != nil {
		s.t.Fatalf("creating account row: %v", err)
	}
	be := fake.New(address, fake.WithClock(s.Clock))
	s.Sup.Adopt(ctx, id, address, be)
	s.accounts[id] = be
	return id
}

func (s *server) backend(accountID string) *fake.Backend {
	s.t.Helper()
	be, ok := s.accounts[accountID]
	if !ok {
		s.t.Fatalf("no fake backend for %s", accountID)
	}
	return be
}

// engine is the core engine for one account, for seeding through the same
// ingest path production uses.
func (s *server) engine(accountID string) *core.Account {
	return &core.Account{
		ID:      accountID,
		Store:   s.Store,
		Backend: s.backend(accountID),
		Clock:   s.Clock,
		Config:  core.DefaultConfig(),
		Source:  "test",
	}
}

// seedConversation puts one thread on the fake AND in the store, in that
// order: a message row carries a foreign key onto its conversation, and
// backfill and the sweep both upsert the thread before fetching its messages
// for exactly that reason.
func (s *server) seedConversation(accountID, sourceID string) store.Conversation {
	s.t.Helper()
	be := s.backend(accountID)
	c := gm.Conversation{
		SourceID:          sourceID,
		Name:              sourceID,
		Type:              gm.ConversationTypeRCS,
		SendModeRaw:       gm.SendModeAuto,
		Folder:            gm.FolderInbox,
		DefaultOutgoingID: "me@" + be.Address(),
		LastActivity:      s.Clock.Now(),
		Participants: []gm.Participant{
			{SourceID: "me@" + be.Address(), IsMe: true, IsVisible: true},
			{SourceID: "part-a", PhoneE164: fictionalA, IsVisible: true},
		},
	}
	be.SeedConversation(c)
	id, err := s.engine(accountID).Ingest().IngestConversation(context.Background(), c)
	if err != nil {
		s.t.Fatalf("seeding conversation: %v", err)
	}
	row, err := s.Store.Conversation(context.Background(), id)
	if err != nil {
		s.t.Fatalf("reading seeded conversation: %v", err)
	}
	return row
}

// seedMessage ingests one message at an exact instant, so a test can put two
// messages on the SAME millisecond and assert the ordering holds anyway.
func (s *server) seedMessage(accountID, convSourceID, sourceID string, at time.Time, outgoing bool) store.Message {
	s.t.Helper()
	m := gm.Message{
		SourceID:       sourceID,
		ConversationID: convSourceID,
		ParticipantID:  "part-a",
		Text:           "message " + sourceID,
		Timestamp:      at,
		StatusRaw:      100, // INCOMING_COMPLETE
		Kind:           gm.MessageKindMessage,
		DeliveryState:  gm.DeliveryStateReceived,
	}
	if outgoing {
		be := s.backend(accountID)
		m.ParticipantID = "me@" + be.Address()
		m.StatusRaw = 1 // OUTGOING_COMPLETE
		m.DeliveryState = gm.DeliveryStateSent
	}
	s.backend(accountID).SeedMessage(m)
	if _, err := s.engine(accountID).Ingest().IngestMessage(context.Background(), m, true, false); err != nil {
		s.t.Fatalf("seeding message: %v", err)
	}
	id := store.MessageID(accountID, convSourceID, sourceID)
	row, err := s.Store.Message(context.Background(), id)
	if err != nil {
		s.t.Fatalf("reading seeded message: %v", err)
	}
	return row
}

// --- HTTP ---------------------------------------------------------------

// envelope is the section 7.1 success or error shape, parsed loosely so a
// test can assert on either without knowing which it got.
type envelope struct {
	Status     int
	Data       map[string]any
	NextCursor *string
	Warnings   []string
	RequestID  string
	Error      *struct {
		Code      string         `json:"code"`
		Message   string         `json:"message"`
		Retryable bool           `json:"retryable"`
		Details   map[string]any `json:"details"`
	}
	Raw     []byte
	Headers http.Header
}

// items is the rows of a listing. **A listing puts its rows in `data.items`**
// (spec section 7.1), so this is the one place a test reaches for them.
func (e envelope) items() []any {
	raw, ok := e.Data["items"]
	if !ok {
		return nil
	}
	list, _ := raw.([]any)
	return list
}

func (e envelope) detail(key string) any {
	if e.Error == nil {
		return nil
	}
	return e.Error.Details[key]
}

// call makes a request with the admin session's token.
func (s *server) call(method, path string, body any) envelope {
	s.t.Helper()
	return s.callWith(s.Token, method, path, body, nil)
}

// callWith makes a request with an explicit bearer and optional extra
// headers, so a test can present an upload token, a download ticket, or a
// hostile Host.
func (s *server) callWith(token, method, path string, body any, headers map[string]string) envelope {
	s.t.Helper()
	var reader io.Reader
	if body != nil {
		switch v := body.(type) {
		case []byte:
			reader = bytes.NewReader(v)
		case string:
			reader = strings.NewReader(v)
		default:
			encoded, err := json.Marshal(v)
			if err != nil {
				s.t.Fatalf("encoding body: %v", err)
			}
			reader = bytes.NewReader(encoded)
		}
	}
	req, err := http.NewRequest(method, s.HTTP.URL+path, reader)
	if err != nil {
		s.t.Fatalf("building request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		if k == "Host" {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	resp, err := s.HTTP.Client().Do(req)
	if err != nil {
		s.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		s.t.Fatalf("reading response: %v", err)
	}

	env := envelope{Status: resp.StatusCode, Raw: raw, Headers: resp.Header}
	var parsed struct {
		Data       json.RawMessage `json:"data"`
		NextCursor *string         `json:"next_cursor"`
		Warnings   []string        `json:"warnings"`
		RequestID  string          `json:"request_id"`
		Error      *struct {
			Code      string         `json:"code"`
			Message   string         `json:"message"`
			Retryable bool           `json:"retryable"`
			Details   map[string]any `json:"details"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &parsed) == nil {
		env.NextCursor = parsed.NextCursor
		env.Warnings = parsed.Warnings
		env.RequestID = parsed.RequestID
		env.Error = parsed.Error
		if len(parsed.Data) > 0 {
			_ = json.Unmarshal(parsed.Data, &env.Data)
		}
	}
	return env
}

// ok fails the test unless the response was a success with the wanted status.
func (e envelope) ok(t *testing.T, want int) envelope {
	t.Helper()
	if e.Status != want {
		t.Fatalf("status %d, want %d: %s", e.Status, want, truncate(e.Raw))
	}
	if e.Error != nil {
		t.Fatalf("unexpected error %s: %s", e.Error.Code, e.Error.Message)
	}
	return e
}

// refused fails the test unless the response was an error with the wanted
// code. The CODE is asserted, never the message: the code is the field a
// client branches on and the message is for a human and may be reworded.
func (e envelope) refused(t *testing.T, code string) envelope {
	t.Helper()
	if e.Error == nil {
		t.Fatalf("expected %s, got a success: %s", code, truncate(e.Raw))
	}
	if e.Error.Code != code {
		t.Fatalf("error code %q, want %q (%s)", e.Error.Code, code, e.Error.Message)
	}
	return e
}

func truncate(b []byte) string {
	if len(b) > 600 {
		return string(b[:600]) + "…"
	}
	return string(b)
}

// key returns a fresh idempotency key. Every mutation needs one (spec section
// 6.3), and a test that reused one by accident would be testing replay.
func key(name string) string { return "idem-" + name }

// getString digs a string out of a decoded envelope.
func getString(t *testing.T, m map[string]any, path ...string) string {
	t.Helper()
	cur := any(m)
	for _, p := range path {
		obj, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("%v: %s is not an object", path, p)
		}
		cur = obj[p]
	}
	s, ok := cur.(string)
	if !ok {
		t.Fatalf("%v is %T, want a string", path, cur)
	}
	return s
}

func countRows(t *testing.T, st *store.Store, query string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := st.Reader().QueryRowContext(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("counting (%s): %v", query, err)
	}
	return n
}

var _ = fmt.Sprintf

// seedAttachment puts real bytes on the fake and an attachment row in the
// store, wired to the same media ID, so a download actually has something to
// fetch and a digest actually has something to hash.
func (s *server) seedAttachment(accountID, convSourceID, msgSourceID string, data []byte, mime string) store.Attachment {
	s.t.Helper()
	ctx := context.Background()
	be := s.backend(accountID)
	ref, err := be.Upload(ctx, data, "photo.jpg", mime)
	if err != nil {
		s.t.Fatalf("seeding media on the fake: %v", err)
	}
	m := gm.Message{
		SourceID:       msgSourceID,
		ConversationID: convSourceID,
		ParticipantID:  "part-a",
		Timestamp:      s.Clock.Now(),
		StatusRaw:      100,
		Kind:           gm.MessageKindMessage,
		DeliveryState:  gm.DeliveryStateReceived,
		Attachments: []gm.Attachment{{
			PartIndex:     0,
			MediaID:       ref.MediaID,
			DecryptionKey: ref.DecryptionKey,
			Filename:      "photo.jpg",
			MimeType:      mime,
			SizeBytes:     int64(len(data)),
		}},
	}
	be.SeedMessage(m)
	eng := s.engine(accountID)
	key := s.Deps.DataKey
	eng.DataKey = &key
	if _, err := eng.Ingest().IngestMessage(ctx, m, true, false); err != nil {
		s.t.Fatalf("seeding attachment message: %v", err)
	}
	msgID := store.MessageID(accountID, convSourceID, msgSourceID)
	atts, err := s.Store.AttachmentsForMessage(ctx, msgID)
	if err != nil || len(atts) == 0 {
		s.t.Fatalf("attachment row was not written: %v", err)
	}
	// Google reports an incoming attachment as available once its bytes are
	// fetchable; the fake has them, so the row says so.
	if err := s.Store.SetAttachmentDownloadState(ctx, atts[0].ID, store.DownloadStateAvailable, ""); err != nil {
		s.t.Fatalf("marking the attachment available: %v", err)
	}
	row, err := s.Store.Attachment(ctx, atts[0].ID)
	if err != nil {
		s.t.Fatalf("re-reading the attachment: %v", err)
	}
	return row
}

// close shuts a server down so the same directory can be reopened, which is
// how a restart is modelled.
func (s *server) close() {
	s.t.Helper()
	s.HTTP.Close()
	if err := s.Store.Close(); err != nil {
		s.t.Fatalf("closing the store: %v", err)
	}
}

// auditRows reads the audit trail from the table it really lives in.
func (s *server) auditRows(kind string) []store.AuditEvent {
	s.t.Helper()
	rows, err := s.Store.ListAuditEvents(context.Background(), store.AuditQuery{
		Kind: kind, Limit: store.MaxLimit,
	})
	if err != nil {
		s.t.Fatalf("listing audit events: %v", err)
	}
	return rows
}

// addAccountRecordingReactions registers an account whose backend records the
// ReactionAction each React carried, which is what "SWITCH sent" needs.
func (s *server) addAccountRecordingReactions(address string) (string, *reactionRecorder) {
	s.t.Helper()
	ctx := context.Background()
	id := store.AccountID(address)
	if err := s.Store.UpsertAccount(ctx, store.Account{
		ID: id, GoogleAccount: address, State: store.StateConnected,
		SessionPresent: true, PairedAtMS: s.Clock.Now().UnixMilli(),
	}); err != nil {
		s.t.Fatalf("creating account row: %v", err)
	}
	be := fake.New(address, fake.WithClock(s.Clock))
	recorder := &reactionRecorder{Backend: be}
	s.Sup.Adopt(ctx, id, address, recorder)
	s.accounts[id] = be
	return id, recorder
}

// reactionsOf reads the reaction rows on one message.
func (s *server) reactionsOf(t *testing.T, messageID string) []store.Reaction {
	t.Helper()
	rows, err := s.Store.ReactionsForMessage(context.Background(), messageID)
	if err != nil {
		t.Fatalf("reading reactions: %v", err)
	}
	return rows
}
