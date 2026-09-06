package cli

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// mutate is the generic executor for a write that returns an operation: it
// warns before an sms_mms wait, sends, waits when asked, and emits once.
func (r *runner) mutate(inv *invocation) error {
	req, err := r.request(inv, inv.cmd.route)
	if err != nil {
		return err
	}
	target, wait, err := r.waitTarget(inv)
	if err != nil {
		return err
	}
	if wait {
		r.warnIfSMS(r.conversationOf(inv), target)
	}

	resp, err := r.client.Do(r.ctx, *req)
	if err != nil {
		return err
	}
	resp, err = r.maybeWait(inv, resp)
	if err != nil {
		return err
	}
	return r.out.Emit(resp)
}

// conversationOf finds the conversation a command targets, for the sms_mms
// warning.
func (r *runner) conversationOf(inv *invocation) string {
	for i, p := range inv.cmd.pos {
		if p.param == "conversation_id" {
			return inv.positional(i)
		}
	}
	return inv.str("--conversation")
}

// messagesList picks the route the positional selects: with a conversation ID
// it is the per-conversation listing, without one it is every account's
// messages, or one account's with --account (spec section 11.3).
func (r *runner) messagesList(inv *invocation) error {
	route := "messages_list"
	scoped := *inv
	if convID := inv.positional(0); convID != "" {
		route = "conversation_messages_list"
		clone := *inv.cmd
		clone.pos = []posDef{{name: "<conv-id>", param: "conversation_id", where: wPath, required: true}}
		scoped.cmd = &clone
	}
	req, err := r.request(&scoped, route)
	if err != nil {
		return err
	}
	if inv.boolean("--all") {
		return r.emitAllPages(*req)
	}
	resp, err := r.client.Do(r.ctx, *req)
	if err != nil {
		return err
	}
	return r.out.Emit(resp)
}

// messagesSend is the one command that does the whole media dance for the
// owner: reserve, PUT, send, in one process, never surfacing an `upl_` ID
// (spec sections 10.2, 11.3). `--file` takes ONE path, matching the
// one-attachment-per-message limit.
func (r *runner) messagesSend(inv *invocation) error {
	if !inv.has("--text") && !inv.has("--file") {
		return usageErr("`agm messages send` needs --text, --file, or both")
	}
	convID := inv.positional(0)

	target, wait, err := r.waitTarget(inv)
	if err != nil {
		return err
	}
	if wait {
		r.warnIfSMS(convID, target)
	}

	req, err := r.request(inv, "messages_send")
	if err != nil {
		return err
	}
	body, _ := req.Body.(map[string]any)
	if body == nil {
		body = map[string]any{}
	}

	if path := inv.str("--file"); path != "" {
		uploadID, err := r.uploadFile(path)
		if err != nil {
			return err
		}
		// An array that currently accepts exactly one element (section 10.2).
		body["upload_ids"] = []string{uploadID}
	}
	req.Body = body

	resp, err := r.client.Do(r.ctx, *req)
	if err != nil {
		return err
	}
	resp, err = r.maybeWait(inv, resp)
	if err != nil {
		return err
	}
	return r.out.Emit(resp)
}

// uploadFile reserves, PUTs and returns the upload ID, which the caller puts
// in the send and nothing prints. The `upl_` ID is not a thing a human
// inspects or cancels, which is why the two upload routes have no command of
// their own (section 11.3).
func (r *runner) uploadFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", flagErr("--file", "%s could not be read: %v", path, err)
	}
	sum := sha256.Sum256(data)

	reserve, err := r.client.Do(r.ctx, Request{
		Method: http.MethodPost,
		Path:   "/v1/uploads",
		Body: map[string]any{
			"filename":   filepath.Base(path),
			"mime_type":  sniffMIME(path, data),
			"size_bytes": len(data),
			"sha256":     hex.EncodeToString(sum[:]),
		},
		IdempotencyKey: r.idempotencyKey(),
	})
	if err != nil {
		return "", err
	}

	var ticket struct {
		UploadID  string `json:"upload_id"`
		UploadURL string `json:"upload_url"`
		Token     string `json:"token"`
		Limits    struct {
			MimeType string `json:"mime_type"`
		} `json:"limits"`
	}
	if err := json.Unmarshal(reserve.Data, &ticket); err != nil || ticket.UploadID == "" {
		return "", &ContractError{Msg: "POST /v1/uploads answered without an upload ticket"}
	}
	r.out.Verbosef("reserved an upload for %s (%d bytes)", filepath.Base(path), len(data))

	contentType := ticket.Limits.MimeType
	if contentType == "" {
		contentType = sniffMIME(path, data)
	}
	// The upload token authenticates this one PUT, and it travels in the
	// Authorization header -- never in the URL (spec section 10.3).
	if _, err := r.client.Do(r.ctx, Request{
		Method:      http.MethodPut,
		Path:        "/v1/uploads/" + ticket.UploadID + "/content",
		AbsoluteURL: ticket.UploadURL,
		RawBody:     data,
		ContentType: contentType,
		Token:       ticket.Token,
	}); err != nil {
		return "", err
	}
	return ticket.UploadID, nil
}

func sniffMIME(path string, data []byte) string {
	if t := mime.TypeByExtension(filepath.Ext(path)); t != "" {
		return strings.TrimSpace(strings.Split(t, ";")[0])
	}
	head := data
	if len(head) > 512 {
		head = head[:512]
	}
	return strings.TrimSpace(strings.Split(http.DetectContentType(head), ";")[0])
}

// removeReaction addresses a reaction by emoji or by react_ ID. There is no
// `unreact`: remove-reaction is remove_reaction is DELETE .../reactions/{emoji}
// (spec section 11.3).
func (r *runner) removeReaction(inv *invocation) error {
	route := "reactions_remove"
	if inv.has("--reaction") {
		route = "reactions_remove_by_id"
		clone := *inv.cmd
		clone.pos = nil
		scoped := *inv
		scoped.cmd = &clone
		inv = &scoped
	} else if inv.positional(0) == "" || inv.positional(1) == "" {
		return usageErr("`agm messages remove-reaction` takes <msg-id> <emoji>, " +
			"or --reaction <react-id>")
	}
	scoped := *inv
	req, err := r.request(&scoped, route)
	if err != nil {
		return err
	}
	resp, err := r.client.Do(r.ctx, *req)
	if err != nil {
		return err
	}
	resp, err = r.maybeWait(inv, resp)
	if err != nil {
		return err
	}
	return r.out.Emit(resp)
}

// download redeems a download ticket. The ticket goes in the Authorization
// header and never in the URL (spec section 10.3), and `--output` names the
// file rather than the format here, which is the one place the two meanings
// of that flag part company (docs/cli.md).
func (r *runner) download(inv *invocation) error {
	attachmentID := inv.positional(0)
	meta, err := r.client.Do(r.ctx, Request{
		Method: http.MethodGet,
		Path:   "/v1/attachments/" + attachmentID,
	})
	if err != nil {
		return err
	}
	var att struct {
		Filename    string `json:"filename"`
		Token       string `json:"token"`
		DownloadURL string `json:"download_url"`
	}
	if err := json.Unmarshal(meta.Data, &att); err != nil {
		return &ContractError{Msg: "GET /v1/attachments/{id} answered without attachment metadata"}
	}

	content, err := r.client.Do(r.ctx, Request{
		Method:      http.MethodGet,
		Path:        "/v1/attachments/" + attachmentID + "/content",
		AbsoluteURL: att.DownloadURL,
		Token:       att.Token,
		Accept:      "*/*",
	})
	if err != nil {
		return err
	}

	dest := inv.str("--output")
	if dest == "" {
		dest = att.Filename
	}
	if dest == "" {
		dest = attachmentID
	}
	if err := os.WriteFile(dest, content.Raw, 0o600); err != nil {
		return localErr(err, "%s could not be written", dest)
	}
	// The path is progress, not the result: stderr, so `--json` stays one
	// value.
	r.out.Infof("wrote %d bytes to %s", len(content.Raw), dest)
	return r.out.Emit(meta)
}

// session is the account's state, or its event stream with --watch.
func (r *runner) session(inv *invocation) error {
	accountID := inv.str("--account")
	if accountID == "" {
		accountID = r.env.Getenv("AGENT_GM_ACCOUNT")
	}

	if inv.boolean("--watch") {
		path := "/v1/accounts/events"
		if accountID != "" {
			path = "/v1/accounts/" + accountID + "/events"
		}
		return r.watch(path)
	}
	if accountID == "" {
		return flagErr("--account", "is required by `agm session` without --watch; "+
			"the all-accounts form is `agm session --watch`")
	}
	resp, err := r.client.Do(r.ctx, Request{Method: http.MethodGet, Path: "/v1/accounts/" + accountID})
	if err != nil {
		return err
	}
	return r.out.Emit(resp)
}

// watch consumes the SSE route. It is the one streaming command, so it is the
// one place stdout carries more than a single value: it writes one JSON
// object per event, which is what `--output jsonl` means everywhere else.
func (r *runner) watch(path string) error {
	req, err := r.client.build(r.ctx, Request{
		Method: http.MethodGet, Path: path, Accept: "text/event-stream",
	})
	if err != nil {
		return err
	}
	httpClient := r.client.HTTP
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	// A stream is not bounded by the request timeout: it is meant to sit
	// open. The context is what ends it.
	resp, err := httpClient.Do(req)
	if err != nil {
		return &TransportError{Op: "GET " + path, Err: err}
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
		return errorFromEnvelope(resp.StatusCode, raw)
	}

	r.out.Infof("watching %s; Ctrl-C to stop", path)
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	event := ""
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if payload == "" {
				continue
			}
			_, _ = fmt.Fprintf(r.out.stdout, "{\"event\":%q,\"data\":%s}\n", event, payload)
		case strings.HasPrefix(line, ":"):
			// A comment, which is how SSE keeps a connection alive.
			r.out.Verbosef("stream keepalive")
		}
	}
	if err := scanner.Err(); err != nil {
		return &TransportError{Op: "reading " + path, Err: err}
	}
	return nil
}

// operationsWait polls an operation on the client side and takes its default
// bound from the server's operations.wait_timeout (spec section 11.3).
func (r *runner) operationsWait(inv *invocation) error {
	id := inv.positional(0)
	target, _, err := r.waitTarget(inv)
	if err != nil {
		return err
	}
	if !r.g.timeoutSet {
		if d := r.serverWaitTimeout(); d > 0 {
			r.g.timeout = d
			r.g.timeoutSet = true
		}
	}

	op, _, err := r.readOperation(id)
	if err != nil {
		return err
	}
	if op.ConversationID != nil {
		r.warnIfSMS(*op.ConversationID, target)
	}
	final, err := r.waitLoop(op, target)
	if err != nil {
		return err
	}
	return r.out.Emit(&Response{Status: http.StatusOK, Data: final})
}

// serverWaitTimeout reads operations.wait_timeout from the settings route. A
// caller without `admin` cannot read it, which is not an error: the default
// stands.
func (r *runner) serverWaitTimeout() time.Duration {
	resp, err := r.client.Do(r.ctx, Request{Method: http.MethodGet, Path: "/v1/admin/settings"})
	if err != nil {
		r.out.Verbosef("could not read operations.wait_timeout: %v", err)
		return 0
	}
	var settings struct {
		Items []struct {
			Key   string          `json:"key"`
			Value json.RawMessage `json:"value"`
		} `json:"items"`
	}
	if err := json.Unmarshal(resp.Data, &settings); err != nil {
		return 0
	}
	for _, s := range settings.Items {
		if s.Key != "operations.wait_timeout" {
			continue
		}
		var asString string
		if err := json.Unmarshal(s.Value, &asString); err == nil {
			if d, err := parseServerDuration(asString); err == nil {
				return d
			}
		}
		var asSeconds float64
		if err := json.Unmarshal(s.Value, &asSeconds); err == nil && asSeconds > 0 {
			return time.Duration(asSeconds) * time.Second
		}
	}
	return 0
}

func parseServerDuration(s string) (time.Duration, error) {
	if n, err := strconv.Atoi(s); err == nil {
		return time.Duration(n) * time.Second, nil
	}
	return time.ParseDuration(s)
}

// authLogin is the admin bootstrap of spec section 12.1: the secret arrives
// over --secret-stdin or a TTY prompt and NEVER in argv.
//
// The credentials write is proved possible before the exchange, for the
// reason section 11.5 gives: a session minted and then not stored is a
// credential lost.
func (r *runner) authLogin(inv *invocation) error {
	if !inv.boolean("--admin") {
		return usageErr("Slice 2 has only the admin path: pass --admin. The OAuth flow that " +
			"--no-browser belongs to arrives in Slice 3")
	}

	secret, err := r.readSecret(inv)
	if err != nil {
		return err
	}

	body := map[string]any{"secret": secret}
	if scopes := inv.str("--scopes"); scopes != "" {
		body["scopes"] = strings.Fields(strings.ReplaceAll(scopes, ",", " "))
	}

	// Prove the destination writable first. Nothing has been minted yet.
	pending, err := r.store.Begin()
	if err != nil {
		return err
	}
	defer pending.Close()

	resp, err := r.client.Do(r.ctx, Request{
		Method: http.MethodPost, Path: "/v1/auth/admin-session", Body: body,
	})
	if err != nil {
		return err
	}

	var session struct {
		AccessToken     string   `json:"access_token"`
		RefreshToken    string   `json:"refresh_token"`
		Scopes          []string `json:"scopes"`
		ExpiresAt       string   `json:"expires_at"`
		AuthorizationID string   `json:"authorization_id"`
	}
	if err := json.Unmarshal(resp.Data, &session); err != nil || session.AccessToken == "" {
		return &ContractError{Msg: "POST /v1/auth/admin-session answered without an access_token"}
	}

	if err := r.store.SaveProfile(pending, r.cred.Profile, Profile{
		Server:          r.cred.Server,
		AccessToken:     session.AccessToken,
		RefreshToken:    session.RefreshToken,
		Scopes:          session.Scopes,
		ExpiresAt:       session.ExpiresAt,
		AuthorizationID: session.AuthorizationID,
		Issuer:          r.cred.Server,
	}); err != nil {
		return err
	}

	r.out.Infof("Logged in as authorization %s", session.AuthorizationID)
	if len(session.Scopes) > 0 {
		r.out.Infof("Scopes: %s", strings.Join(session.Scopes, " "))
	}
	// The tokens are stored, not printed: stdout is a transcript, and a
	// transcript is not where a bearer token belongs (spec section 12.1).
	return r.out.Emit(&Response{
		Status:    resp.Status,
		Data:      redactTokens(resp.Data),
		Warnings:  resp.Warnings,
		RequestID: resp.RequestID,
	})
}

// readSecret reads AGENT_GM_ADMIN_SECRET from stdin or a prompt. It is never
// a flag value: a secret must not appear in argv (spec section 12.1).
func (r *runner) readSecret(inv *invocation) (string, error) {
	if !inv.boolean("--secret-stdin") {
		_, _ = fmt.Fprint(r.env.Stderr, "Admin secret: ")
	}
	reader := bufio.NewReader(r.env.Stdin)
	line, err := reader.ReadString('\n')
	secret := strings.TrimSpace(line)
	if secret == "" {
		if err != nil && err != io.EOF {
			return "", localErr(err, "the admin secret could not be read")
		}
		return "", usageErr("no admin secret was given; pass it on stdin with --secret-stdin")
	}
	if !inv.boolean("--secret-stdin") {
		_, _ = fmt.Fprintln(r.env.Stderr)
	}
	return secret, nil
}

func redactTokens(data json.RawMessage) json.RawMessage {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err != nil {
		return data
	}
	for _, key := range []string{"access_token", "refresh_token"} {
		if _, ok := obj[key]; ok {
			obj[key] = json.RawMessage(`"(stored in the credentials file)"`)
		}
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return data
	}
	return out
}

// authLogout ends this token's session and forgets the profile. It has
// nothing to do with signing a Google account out (spec section 7.5).
func (r *runner) authLogout(inv *invocation) error {
	if err := r.confirm(inv, inv.cmd.confirm); err != nil {
		return err
	}
	resp, err := r.client.Do(r.ctx, Request{Method: http.MethodPost, Path: "/v1/auth/logout"})
	if err != nil {
		return err
	}
	pending, err := r.store.Begin()
	if err != nil {
		return err
	}
	defer pending.Close()
	if err := r.store.DeleteProfile(pending, r.cred.Profile); err != nil {
		return err
	}
	r.out.Infof("the profile %s no longer holds a token", r.cred.Profile)
	return r.out.Emit(resp)
}

// settingsSet takes one or more key=value pairs and sends them as ONE PATCH,
// so section 7.7's whole-body validation applies: any invalid key rejects the
// command and changes nothing.
func (r *runner) settingsSet(inv *invocation) error {
	if len(inv.pos) == 0 {
		return usageErr("`agm admin settings set` takes one or more <key>=<value> pairs")
	}
	body := map[string]any{}
	for _, pair := range inv.pos {
		key, value, ok := strings.Cut(pair, "=")
		if !ok || key == "" {
			return usageErr("%q is not a <key>=<value> pair", pair)
		}
		// A value that is JSON is sent as JSON, so a number stays a number
		// and `true` is a boolean rather than the string "true".
		var parsed any
		if err := json.Unmarshal([]byte(value), &parsed); err == nil {
			body[key] = parsed
			continue
		}
		body[key] = value
	}
	resp, err := r.client.Do(r.ctx, Request{
		Method: http.MethodPatch, Path: "/v1/admin/settings", Body: body,
	})
	if err != nil {
		return err
	}
	return r.out.Emit(resp)
}

// backfill is destructive only in its widest form: with neither --account nor
// --conversation it walks every account, which is the most expensive
// operation Agent GM offers (spec sections 5.4, 11.3).
func (r *runner) backfill(inv *invocation) error {
	whole := !inv.has("--account") && !inv.has("--conversation")
	if whole {
		if err := r.confirm(inv, inv.cmd.confirm); err != nil {
			return err
		}
	}
	body := map[string]any{}
	if v := inv.str("--account"); v != "" {
		body["account_id"] = v
	}
	if v := inv.str("--conversation"); v != "" {
		body["conversation_id"] = v
	}
	if whole {
		body["confirm"] = true
	}
	resp, err := r.client.Do(r.ctx, Request{
		Method: http.MethodPost, Path: "/v1/admin/backfill", Body: body,
	})
	if err != nil {
		return err
	}
	return r.out.Emit(resp)
}
