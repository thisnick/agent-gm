package mcp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/thisnick/agent-gm/internal/accounts"
	"github.com/thisnick/agent-gm/internal/api"
	"github.com/thisnick/agent-gm/internal/audit"
	"github.com/thisnick/agent-gm/internal/authz"
	"github.com/thisnick/agent-gm/internal/clock"
	"github.com/thisnick/agent-gm/internal/core"
	"github.com/thisnick/agent-gm/internal/gm"
	"github.com/thisnick/agent-gm/internal/gm/fake"
	"github.com/thisnick/agent-gm/internal/mcp"
	"github.com/thisnick/agent-gm/internal/media"
	"github.com/thisnick/agent-gm/internal/settings"
	"github.com/thisnick/agent-gm/internal/store"
)

// The harness for the Slice 3 MCP acceptance tests this package owns: 14, 15,
// 16, 17, 19, 20, 21, 22 and the MCP half of 28.
//
// Nothing here needs a phone, a network or Docker, so none of these tests is
// behind a gate.
//
// **ONE FAKE IS ONE ACCOUNT** (spec section 13.1). Test 17 registers two
// fakes with one supervisor over one store, which is exactly the shape
// production has.
//
// **Fictional numbers only.** `+1 202 555 01xx` is reserved for fiction.

const (
	fictionalA = "+12025550101"

	addressA = "owner-a@example.test"
	addressB = "owner-b@example.test"

	// testDataKey is 64 hex characters and is not a secret: it lives only
	// inside a t.TempDir() that is deleted when the test ends.
	testDataKey = "0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20"

	// testAdminSecret is well over the 43-byte minimum of section 12.1.
	testAdminSecret = "test-admin-secret-not-a-real-one-0123456789abcdefghijklmnop"

	// publicURL is what the Origin check compares against and what every URL
	// the server hands out is built from.
	publicURL = "https://gm.example.test"

	testVersion = "0.0.0-test"
	testCommit  = "abc1234"
	testSource  = "https://github.com/thisnick/agent-gm"
)

// harness is a whole fake-backed Agent GM with its MCP endpoint mounted, in
// process, over httptest -- which is a RUNNING SERVER: section 16 Slice 3
// test 15 requires `tools/list` to be fetched from one rather than read off a
// table in the same process's memory.
type harness struct {
	t     *testing.T
	Dir   string
	Clock *clock.Fake
	Store *store.Store
	Deps  *api.HandlerDeps
	API   *api.Server
	MCP   *mcp.Handler
	Sup   *accounts.Supervisor
	HTTP  *httptest.Server

	// Token is an admin session carrying admin plus the three messaging
	// scopes (spec section 9.7). Until the OAuth flow lands this is how a
	// test mints a credential; the credential SHAPE is the same either way.
	Token string

	accounts map[string]*fake.Backend
	sessions map[string]*sdk.ClientSession
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	return newHarnessAt(t, publicURL)
}

// newHarnessAt builds the harness with a chosen AGENT_GM_PUBLIC_URL, for the
// tests that are about what that URL's SHAPE does -- a path on it, a trailing
// slash on it -- rather than about its value.
func newHarnessAt(t *testing.T, publicURL string) *harness {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	clk := clock.NewFake()

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
		Audit:                 audit.NewWriter(api.NewStoreAppender(st), clk),
		Signer:                signer,
		Cache:                 &media.Cache{Dir: dir + "/media-cache", Index: media.NewStoreIndex(st), MaxBytes: 2 << 30},
		Clock:                 clk,
		DataKey:               key,
		DataDir:               dir,
		PublicURL:             publicURL,
		Version:               testVersion,
		Commit:                testCommit,
		SourceURLBase:         testSource,
		ConfigVersionCompiled: gm.CompiledConfigVersion().String(),
		UpstreamCommit:        gm.PinnedUpstreamCommit,
		Core:                  core.DefaultConfig(),
		Heartbeat:             50 * time.Millisecond,
	}

	apiServer := api.NewServer(api.Deps{Authz: authzService, PublicURL: publicURL})
	if err := api.RegisterAll(apiServer, deps); err != nil {
		t.Fatalf("registering handlers: %v", err)
	}
	mcpHandler := mcp.New(mcp.Config{
		API:       apiServer,
		Authz:     authzService,
		PublicURL: publicURL,
		Version:   testVersion,
		Commit:    testCommit,
		SourceURL: testSource + "/tree/" + testCommit,
	})
	httpServer := httptest.NewServer(mcp.Mount(apiServer, mcpHandler))
	t.Cleanup(httpServer.Close)

	sess, err := authzService.MintAdminSession(ctx, testAdminSecret, nil, "127.0.0.1")
	if err != nil {
		t.Fatalf("minting the admin session: %v", err)
	}

	return &harness{
		t: t, Dir: dir, Clock: clk, Store: st, Deps: deps, API: apiServer,
		MCP: mcpHandler, Sup: sup, HTTP: httpServer, Token: sess.AccessToken,
		accounts: map[string]*fake.Backend{},
	}
}

// narrowToken mints a second admin session narrowed to the given scopes,
// which is how a scope refusal is produced before the OAuth flow exists
// (spec section 9.7).
func (h *harness) narrowToken(scopes ...string) string {
	h.t.Helper()
	sess, err := h.Deps.Authz.MintAdminSession(context.Background(), testAdminSecret, scopes, "127.0.0.1")
	if err != nil {
		h.t.Fatalf("minting a narrowed session: %v", err)
	}
	return sess.AccessToken
}

// addAccount registers one more account: one fake, one row, one supervised
// backend. Calling it twice is how a two-account test is built.
func (h *harness) addAccount(address string) string {
	h.t.Helper()
	ctx := context.Background()
	id := store.AccountID(address)
	if err := h.Store.UpsertAccount(ctx, store.Account{
		ID:             id,
		GoogleAccount:  address,
		State:          store.StateConnected,
		SessionPresent: true,
		PairedAtMS:     h.Clock.Now().UnixMilli(),
	}); err != nil {
		h.t.Fatalf("creating account row: %v", err)
	}
	be := fake.New(address, fake.WithClock(h.Clock))
	h.Sup.Adopt(ctx, id, address, be)
	h.accounts[id] = be
	return id
}

func (h *harness) backend(accountID string) *fake.Backend {
	h.t.Helper()
	be, ok := h.accounts[accountID]
	if !ok {
		h.t.Fatalf("no fake backend for %s", accountID)
	}
	return be
}

func (h *harness) engine(accountID string) *core.Account {
	return &core.Account{
		ID:      accountID,
		Store:   h.Store,
		Backend: h.backend(accountID),
		Clock:   h.Clock,
		Config:  core.DefaultConfig(),
		Source:  "test",
	}
}

// seedConversation puts one thread on the fake AND in the store.
func (h *harness) seedConversation(accountID, sourceID string) store.Conversation {
	h.t.Helper()
	be := h.backend(accountID)
	c := gm.Conversation{
		SourceID:          sourceID,
		Name:              sourceID,
		Type:              gm.ConversationTypeRCS,
		SendModeRaw:       gm.SendModeAuto,
		Folder:            gm.FolderInbox,
		DefaultOutgoingID: "me@" + be.Address(),
		LastActivity:      h.Clock.Now(),
		Participants: []gm.Participant{
			{SourceID: "me@" + be.Address(), IsMe: true, IsVisible: true},
			{SourceID: "part-a", PhoneE164: fictionalA, IsVisible: true},
		},
	}
	be.SeedConversation(c)
	id, err := h.engine(accountID).Ingest().IngestConversation(context.Background(), c)
	if err != nil {
		h.t.Fatalf("seeding conversation: %v", err)
	}
	row, err := h.Store.Conversation(context.Background(), id)
	if err != nil {
		h.t.Fatalf("reading seeded conversation: %v", err)
	}
	return row
}

// seedMessage ingests one message at an exact instant.
func (h *harness) seedMessage(accountID, convSourceID, sourceID string) store.Message {
	h.t.Helper()
	m := gm.Message{
		SourceID:       sourceID,
		ConversationID: convSourceID,
		ParticipantID:  "part-a",
		Text:           "message " + sourceID,
		Timestamp:      h.Clock.Now(),
		StatusRaw:      100, // INCOMING_COMPLETE
		Kind:           gm.MessageKindMessage,
		DeliveryState:  gm.DeliveryStateReceived,
	}
	h.backend(accountID).SeedMessage(m)
	if _, err := h.engine(accountID).Ingest().IngestMessage(context.Background(), m, true, false); err != nil {
		h.t.Fatalf("seeding message: %v", err)
	}
	row, err := h.Store.Message(context.Background(), store.MessageID(accountID, convSourceID, sourceID))
	if err != nil {
		h.t.Fatalf("reading seeded message: %v", err)
	}
	return row
}

// --- the MCP wire -------------------------------------------------------------

// rpcAnswer is a parsed JSON-RPC answer plus the HTTP facts a transport test
// needs.
type rpcAnswer struct {
	Status  int
	Headers http.Header
	Raw     []byte
	Result  map[string]any
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
}

// --- the reference client ------------------------------------------------------
//
// Every well-formed call below goes through the OFFICIAL MCP Go SDK client
// against the running httptest server, not through a hand-rolled poster
// (decision D36). That is the point: the reference implementation on both
// ends means a result these tests accept is a result a real connector
// accepts, and a shape the SDK rejects fails here rather than at a live gate.
// The reviewer's Slice 3 finding R-1 -- a `resources/read` failure the
// official client threw on before the application saw it -- is exactly the
// class of bug a hand-rolled client cannot find.
//
// h.post below stays raw, because the transport tests deliberately send what
// a well-formed call would not.

// bearerClient is an http.Client that presents one token on every request.
type bearerRoundTripper struct {
	token string
	next  http.RoundTripper
}

func (b bearerRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	clone := r.Clone(r.Context())
	if b.token != "" {
		clone.Header.Set("Authorization", "Bearer "+b.token)
	}
	return b.next.RoundTrip(clone)
}

// session connects the SDK client for a token, once per token per harness.
func (h *harness) session(token string) *sdk.ClientSession {
	h.t.Helper()
	if cs, ok := h.sessions[token]; ok {
		return cs
	}
	client := sdk.NewClient(&sdk.Implementation{Name: "agent-gm-tests", Version: "0.0.0"}, nil)
	transport := &sdk.StreamableClientTransport{
		Endpoint:   h.HTTP.URL + mcp.Path,
		HTTPClient: &http.Client{Transport: bearerRoundTripper{token: token, next: http.DefaultTransport}},
	}
	cs, err := client.Connect(context.Background(), transport, nil)
	if err != nil {
		h.t.Fatalf("the SDK client could not connect with this token: %v", err)
	}
	h.t.Cleanup(func() { _ = cs.Close() })
	if h.sessions == nil {
		h.sessions = map[string]*sdk.ClientSession{}
	}
	h.sessions[token] = cs
	return cs
}

// call makes one call with the admin session's token, through the SDK client.
func (h *harness) call(method string, params any) rpcAnswer {
	h.t.Helper()
	return h.callWith(h.Token, method, params, nil)
}

// callWith makes one call with an explicit bearer.
//
// `headers` is accepted for the transport tests that still hand-build a
// request; a call that passes any falls through to the raw poster, because
// the SDK client owns its own headers and overriding them would be testing
// the poster rather than the server.
func (h *harness) callWith(token, method string, params any, headers map[string]string) rpcAnswer {
	h.t.Helper()
	if len(headers) > 0 || token == "" {
		body := map[string]any{"jsonrpc": "2.0", "id": 1, "method": method}
		if params != nil {
			body["params"] = params
		}
		encoded, err := json.Marshal(body)
		if err != nil {
			h.t.Fatalf("encoding the call: %v", err)
		}
		return h.post(token, encoded, headers)
	}

	cs := h.session(token)
	ctx := context.Background()
	var (
		result sdk.Result
		err    error
	)
	switch method {
	case "tools/list":
		result, err = cs.ListTools(ctx, decodeParams[sdk.ListToolsParams](h.t, params))
	case "tools/call":
		result, err = cs.CallTool(ctx, decodeParams[sdk.CallToolParams](h.t, params))
	case "resources/list":
		result, err = cs.ListResources(ctx, decodeParams[sdk.ListResourcesParams](h.t, params))
	case "resources/templates/list":
		result, err = cs.ListResourceTemplates(ctx, decodeParams[sdk.ListResourceTemplatesParams](h.t, params))
	case "resources/read":
		result, err = cs.ReadResource(ctx, decodeParams[sdk.ReadResourceParams](h.t, params))
	case "initialize":
		// The SDK initialises on Connect and keeps the result; there is no
		// second initialize to make, and asking for one would be a protocol
		// error the SDK is right to refuse.
		result = cs.InitializeResult()
	case "ping":
		err = cs.Ping(ctx, nil)
		result = &sdk.CallToolResult{}
	default:
		h.t.Fatalf("the harness has no SDK client call for %q; add one rather than hand-rolling a POST", method)
	}
	return answerOf(h.t, result, err)
}

// decodeParams re-reads a test's params map as the SDK's typed params, so a
// test still writes the wire shape it is asserting about.
func decodeParams[T any](t *testing.T, params any) *T {
	t.Helper()
	var out T
	if params == nil {
		return &out
	}
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("encoding params: %v", err)
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decoding params as %T: %v", out, err)
	}
	return &out
}

// answerOf renders a typed SDK result back into the wire object the
// assertions read, and a JSON-RPC error into the error half.
func answerOf(t *testing.T, result sdk.Result, err error) rpcAnswer {
	t.Helper()
	answer := rpcAnswer{Status: http.StatusOK, Headers: http.Header{}}
	if err != nil {
		var jerr *jsonrpc.Error
		if errors.As(err, &jerr) {
			answer.Error = &struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			}{Code: int(jerr.Code), Message: jerr.Message}
			answer.Raw = []byte(jerr.Error())
			return answer
		}
		t.Fatalf("the SDK client failed outside JSON-RPC: %v", err)
	}
	raw, marshalErr := json.Marshal(result)
	if marshalErr != nil {
		t.Fatalf("re-encoding the result: %v", marshalErr)
	}
	answer.Raw = raw
	if unmarshalErr := json.Unmarshal(raw, &answer.Result); unmarshalErr != nil {
		t.Fatalf("re-reading the result: %v", unmarshalErr)
	}
	return answer
}

// post is the raw form, for the transport tests that send something a
// well-formed call would not.
func (h *harness) post(token string, body []byte, headers map[string]string) rpcAnswer {
	h.t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.HTTP.URL+mcp.Path, bytes.NewReader(body))
	if err != nil {
		h.t.Fatalf("building the request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range headers {
		if v == "" {
			req.Header.Del(k)
			continue
		}
		req.Header.Set(k, v)
	}
	resp, err := h.HTTP.Client().Do(req)
	if err != nil {
		h.t.Fatalf("POST /mcp: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		h.t.Fatalf("reading the answer: %v", err)
	}
	answer := rpcAnswer{Status: resp.StatusCode, Headers: resp.Header, Raw: raw}
	// A stateless streamable server answers a POST with a one-event SSE
	// frame unless the client asked for JSON only, so the raw poster has to
	// read both shapes. The SDK client does this for itself; this helper
	// exists for the handful of tests that must send what no client would.
	payload := raw
	if bytes.HasPrefix(bytes.TrimSpace(raw), []byte("event:")) {
		for _, line := range bytes.Split(raw, []byte("\n")) {
			if rest, ok := bytes.CutPrefix(line, []byte("data: ")); ok {
				payload = rest
				break
			}
		}
	}
	var parsed struct {
		Result map[string]any `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(payload, &parsed) == nil {
		answer.Result = parsed.Result
		answer.Error = parsed.Error
	}
	return answer
}

// rawCall posts one hand-built JSON-RPC call. It is for the tests that must
// send something no reference client will send -- an unknown method, an
// explicit protocolVersion -- and for nothing else.
func (h *harness) rawCall(token, method string, params any) rpcAnswer {
	h.t.Helper()
	return h.rawCallWith(token, method, params, nil)
}

// atProtocol is the header a raw call sets to be judged by section 8.1's
// protocol rather than by the oldest one the SDK still supports. It matters:
// an unknown method is answered as a JSON-RPC error only from 2026-07-28
// onwards (mcp/streamable.go:1477).
func atProtocol(version string) map[string]string {
	return map[string]string{"MCP-Protocol-Version": version}
}

func (h *harness) rawCallWith(token, method string, params any, headers map[string]string) rpcAnswer {
	h.t.Helper()
	body := map[string]any{"jsonrpc": "2.0", "id": 1, "method": method}
	if params != nil {
		body["params"] = params
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		h.t.Fatalf("encoding the call: %v", err)
	}
	// From 2026-07-28 the SDK requires the method in a header as well as in
	// the body (SEP-2575). A reference client sets it; a raw poster has to.
	if headers["MCP-Protocol-Version"] >= mcp.ProtocolVersion && headers["Mcp-Method"] == "" {
		withMethod := map[string]string{"Mcp-Method": method}
		for k, v := range headers {
			withMethod[k] = v
		}
		headers = withMethod
	}
	return h.post(token, encoded, headers)
}

// tool calls one tool and returns the result object.
func (h *harness) tool(name string, args map[string]any) map[string]any {
	h.t.Helper()
	return h.toolWith(h.Token, name, args)
}

func (h *harness) toolWith(token, name string, args map[string]any) map[string]any {
	h.t.Helper()
	if args == nil {
		args = map[string]any{}
	}
	answer := h.callWith(token, "tools/call", map[string]any{"name": name, "arguments": args}, nil)
	if answer.Error != nil {
		h.t.Fatalf("tools/call %s answered a JSON-RPC error, which section 8.2 reserves for four cases: %d %s",
			name, answer.Error.Code, answer.Error.Message)
	}
	if answer.Result == nil {
		h.t.Fatalf("tools/call %s answered no result: %s", name, answer.Raw)
	}
	return answer.Result
}

// toolNames is the names `tools/list` returns for a token.
func (h *harness) toolNames(token string) []string {
	h.t.Helper()
	answer := h.callWith(token, "tools/list", nil, nil)
	if answer.Result == nil {
		h.t.Fatalf("tools/list answered no result: %s", answer.Raw)
	}
	list, _ := answer.Result["tools"].([]any)
	out := make([]string, 0, len(list))
	for _, entry := range list {
		obj, _ := entry.(map[string]any)
		name, _ := obj["name"].(string)
		out = append(out, name)
	}
	return out
}

// listedTools is the full `tools/list` payload for a token.
func (h *harness) listedTools(token string) []map[string]any {
	h.t.Helper()
	answer := h.callWith(token, "tools/list", nil, nil)
	if answer.Result == nil {
		h.t.Fatalf("tools/list answered no result: %s", answer.Raw)
	}
	list, _ := answer.Result["tools"].([]any)
	out := make([]map[string]any, 0, len(list))
	for _, entry := range list {
		obj, ok := entry.(map[string]any)
		if !ok {
			h.t.Fatalf("a tools/list entry is not an object: %v", entry)
		}
		out = append(out, obj)
	}
	return out
}

// isError reads a result's isError flag.
func isError(result map[string]any) bool {
	v, _ := result["isError"].(bool)
	return v
}

// structured reads a result's structuredContent.
func structured(t *testing.T, result map[string]any) map[string]any {
	t.Helper()
	sc, ok := result["structuredContent"].(map[string]any)
	if !ok {
		t.Fatalf("the result carries no structuredContent: %v", result)
	}
	return sc
}

// resultError reads the REST error a failed result carries.
func resultError(t *testing.T, result map[string]any) map[string]any {
	t.Helper()
	e, ok := structured(t, result)["error"].(map[string]any)
	if !ok {
		t.Fatalf("the result carries no structuredContent.error: %v", result)
	}
	return e
}

// contentBlocks reads a result's content array.
func contentBlocks(t *testing.T, result map[string]any) []map[string]any {
	t.Helper()
	list, ok := result["content"].([]any)
	if !ok {
		t.Fatalf("the result carries no content array: %v", result)
	}
	out := make([]map[string]any, 0, len(list))
	for _, entry := range list {
		obj, ok := entry.(map[string]any)
		if !ok {
			t.Fatalf("a content block is not an object: %v", entry)
		}
		out = append(out, obj)
	}
	return out
}

// dataOf reads structuredContent.data as an object.
func dataOf(t *testing.T, result map[string]any) map[string]any {
	t.Helper()
	obj, ok := structured(t, result)["data"].(map[string]any)
	if !ok {
		t.Fatalf("structuredContent.data is not an object: %v", result)
	}
	return obj
}

// itemsOf reads structuredContent.data.items.
func itemsOf(t *testing.T, result map[string]any) []map[string]any {
	t.Helper()
	list, ok := dataOf(t, result)["items"].([]any)
	if !ok {
		t.Fatalf("structuredContent.data has no items array: %v", result)
	}
	out := make([]map[string]any, 0, len(list))
	for _, entry := range list {
		obj, _ := entry.(map[string]any)
		out = append(out, obj)
	}
	return out
}

// seedAttachment puts real bytes on the fake and an attachment row in the
// store, wired to the same media ID, so a download actually has something to
// fetch. It makes no state transition of its own: the row's download_state is
// whatever production wrote.
func (h *harness) seedAttachment(accountID, convSourceID, msgSourceID string, data []byte, mime, filename string) store.Attachment {
	h.t.Helper()
	ctx := context.Background()
	be := h.backend(accountID)
	ref, err := be.Upload(ctx, data, filename, mime)
	if err != nil {
		h.t.Fatalf("seeding media on the fake: %v", err)
	}
	m := gm.Message{
		SourceID:       msgSourceID,
		ConversationID: convSourceID,
		ParticipantID:  "part-a",
		Timestamp:      h.Clock.Now(),
		StatusRaw:      100,
		Kind:           gm.MessageKindMessage,
		DeliveryState:  gm.DeliveryStateReceived,
		Attachments: []gm.Attachment{{
			PartIndex:     0,
			MediaID:       ref.MediaID,
			DecryptionKey: ref.DecryptionKey,
			Filename:      filename,
			MimeType:      mime,
			SizeBytes:     int64(len(data)),
		}},
	}
	be.SeedMessage(m)
	eng := h.engine(accountID)
	key := h.Deps.DataKey
	eng.DataKey = &key
	if _, err := eng.Ingest().IngestMessage(ctx, m, true, false); err != nil {
		h.t.Fatalf("seeding attachment message: %v", err)
	}
	msgID := store.MessageID(accountID, convSourceID, msgSourceID)
	atts, err := h.Store.AttachmentsForMessage(ctx, msgID)
	if err != nil || len(atts) == 0 {
		h.t.Fatalf("attachment row was not written: %v", err)
	}
	row, err := h.Store.Attachment(ctx, atts[0].ID)
	if err != nil {
		h.t.Fatalf("re-reading the attachment: %v", err)
	}
	return row
}
