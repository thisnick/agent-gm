package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/thisnick/agent-gm/internal/apierr"
)

// The three effect sentences, aliased from internal/apierr so that the
// confirmation prompt, the route's `effect` field and the MCP tool
// description are literally the same string (spec section 7.7).
//
// They are used for the prompt, which necessarily happens BEFORE the call,
// and then checked against what the route actually returned -- which is what
// the CLI prints. Building the printed sentence locally is the mutation
// section 16 Slice 2 test 28 exists to kill.
const (
	effectMessageDelete      = apierr.EffectMessageDelete
	effectConversationDelete = apierr.EffectConversationDelete
	effectAccountRemove      = apierr.EffectAccountRemove
)

// Env is one invocation's environment. Everything the CLI touches outside its
// own process is here, so a test drives the real command with real argument
// parsing against a stub server rather than calling handlers directly.
type Env struct {
	// Args are the arguments after the program name.
	Args   []string
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	// Getenv reads the environment. Nil means os.Getenv.
	Getenv func(string) string
	// HTTP is the transport. Nil means http.DefaultClient.
	HTTP *http.Client
	// Version is what `agm version` prints.
	Version string
	// PollInterval is how often a wait re-reads an operation. Zero means one
	// second; a test sets it small so a wait is not a sleep.
	PollInterval time.Duration
	// Now is the clock, injected for the same reason.
	Now func() time.Time
}

func (e *Env) normalise() {
	if e.Stdin == nil {
		e.Stdin = strings.NewReader("")
	}
	if e.Stdout == nil {
		e.Stdout = os.Stdout
	}
	if e.Stderr == nil {
		e.Stderr = os.Stderr
	}
	if e.Getenv == nil {
		e.Getenv = os.Getenv
	}
	if e.Now == nil {
		e.Now = time.Now
	}
	if e.PollInterval == 0 {
		e.PollInterval = time.Second
	}
	if e.Version == "" {
		e.Version = "dev"
	}
}

// runner is one invocation.
type runner struct {
	env    *Env
	g      globals
	out    *Output
	client *Client
	store  *Store
	cred   Credential
	ctx    context.Context
}

// Run executes one `agm` invocation and returns its exit code (spec section
// 11.2). It never calls os.Exit, so a test runs the real command.
func Run(env Env) int {
	env.normalise()
	stderr := env.Stderr

	err := dispatch(&env)
	if err == nil {
		return apierr.ExitOK
	}

	code := ExitCodeFor(err)
	_, _ = fmt.Fprintf(stderr, "agm: %s\n", err.Error())

	// The three exit codes spec section 11.2 says are easy to get wrong get
	// a second line, because the message is what stops a script doing the
	// dangerous thing.
	var apiErr *apierr.Error
	if errors.As(err, &apiErr) {
		switch apiErr.Code {
		case apierr.CodePhoneNotResponding:
			_, _ = fmt.Fprintln(stderr, "agm: the operation is PENDING, not failed: the phone may still "+
				"act on it. Do not resend -- wait with `agm operations wait <op-id>`.")
			if id, ok := apiErr.Details["operation_id"].(string); ok && id != "" {
				_, _ = fmt.Fprintf(stderr, "agm: operation %s\n", id)
			}
		case apierr.CodeIdempotencyConflict:
			_, _ = fmt.Fprintln(stderr, "agm: the same idempotency key was presented with a different "+
				"body. Fix the invocation; never retry it unchanged.")
		case apierr.CodeInvalidToken:
			_, _ = fmt.Fprintln(stderr, "agm: log in again with `agm auth login`.")
		}
	}
	return code
}

func dispatch(env *Env) error {
	args := env.Args
	if len(args) == 0 {
		printUsage(env.Stderr)
		return usageErr("no command given")
	}
	switch args[0] {
	case "-h", "--help", "help":
		printUsage(env.Stderr)
		return nil
	}

	cmd, rest, ok := lookup(args)
	if !ok {
		printUsage(env.Stderr)
		return usageErr("%q is not an agm command", strings.Join(args, " "))
	}

	values, set, pos, err := parseArgs(rest, cmd.allFlags())
	if err != nil {
		return err
	}
	inv := &invocation{cmd: cmd, values: values, set: set, pos: pos}
	if set["--help"] {
		printCommandHelp(env.Stderr, cmd)
		return nil
	}
	if err := checkPositionals(inv); err != nil {
		return err
	}

	g, err := resolveGlobals(inv)
	if err != nil {
		return err
	}

	r := &runner{
		env: env,
		g:   g,
		ctx: context.Background(),
		out: &Output{
			stdout:  env.Stdout,
			stderr:  env.Stderr,
			format:  g.format,
			quiet:   g.quiet,
			verbose: g.verbose,
		},
	}

	// A local command drives no route, so it needs no server and no
	// credential: `agm version` on a machine that has never logged in works.
	if cmd.route == "" {
		return cmd.run(r, inv)
	}

	// `agm pair` connects for itself, AFTER it has gathered the cookies, so
	// that a bad paste is diagnosed as a bad paste rather than as a missing
	// server (spec section 11.4). Every other command connects here.
	if cmd.connectsItself {
		return cmd.run(r, inv)
	}
	if err := r.connect(); err != nil {
		return err
	}
	if cmd.run != nil {
		return cmd.run(r, inv)
	}
	return r.generic(inv)
}

// connect resolves the credential and builds the client. It is where spec
// section 11.5's precedence and its write-back safety rule are applied, and
// it happens before any command sends anything.
func (r *runner) connect() error {
	r.store = NewStore(CredentialsPath(r.g.credentialsFile, r.env.Getenv))

	cred, err := ResolveCredential(r.store, r.env.Getenv, r.g.server, r.g.profile)
	if err != nil {
		return err
	}
	if cred.Server == "" {
		return &LocalError{Msg: "no server is configured: pass --server, set AGENT_GM_URL, " +
			"or log in with `agm auth login` so a profile records one"}
	}
	r.cred = cred
	r.out.profile = cred.Profile
	r.out.server = cred.Server

	timeout := r.g.timeout
	if timeout == 0 {
		timeout = DefaultTimeout
	}
	r.client = &Client{
		BaseURL:   cred.Server,
		Token:     cred.Token,
		Timeout:   timeout,
		HTTP:      r.env.HTTP,
		UserAgent: "agm/" + r.env.Version,
	}
	r.out.Verbosef("server %s, profile %s, credential from %s",
		cred.Server, cred.Profile, cred.Source)

	if cred.Source == SourceRefreshFile {
		return r.exchangeRefreshToken()
	}
	return nil
}

// exchangeRefreshToken is the automation credential of spec section 11.5, and
// the order of the three steps below is the whole of the write-back safety
// rule:
//
//  1. Prove the destination writable -- create the temporary file the write
//     will later rename into place -- BEFORE the token is spent. An
//     unwritable directory is exit 9 while the token is still good.
//  2. Only then exchange. A token the server has already refused is exit 3
//     and is never retried: rotation makes it single-use, so a retry cannot
//     succeed and a second attempt only obscures what happened.
//  3. Store the rotated value atomically at 0600.
func (r *runner) exchangeRefreshToken() error {
	token, err := ReadRefreshTokenFile(r.cred.RefreshTokenFile)
	if err != nil {
		return err
	}

	// Step 1. Nothing has been spent at this point.
	pending, err := beginWrite(r.cred.RefreshTokenFile, "")
	if err != nil {
		return err
	}
	defer pending.Close()

	// Step 2.
	resp, err := r.client.Do(r.ctx, Request{
		Method: http.MethodPost,
		Path:   "/v1/auth/refresh",
		Body:   map[string]any{"refresh_token": token},
	})
	if err != nil {
		var apiErr *apierr.Error
		if errors.As(err, &apiErr) && apiErr.Code == apierr.CodeInvalidToken {
			return &apierr.Error{
				Code: apierr.CodeInvalidToken,
				Message: "the stored refresh token was refused; it is single-use and is not " +
					"retried. Log in again to write a new one to " + r.cred.RefreshTokenFile,
				Details: apiErr.Details,
			}
		}
		return err
	}

	var body struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal(resp.Data, &body); err != nil || body.AccessToken == "" {
		return &ContractError{Msg: "POST /v1/auth/refresh answered without an access_token"}
	}

	// Step 3.
	if body.RefreshToken != "" {
		if err := pending.Commit([]byte(body.RefreshToken + "\n")); err != nil {
			return err
		}
	}
	r.client.Token = body.AccessToken
	r.cred.Token = body.AccessToken
	return nil
}

// generic is the executor every command that is only "call this route" uses.
func (r *runner) generic(inv *invocation) error {
	req, err := r.request(inv, inv.cmd.route)
	if err != nil {
		return err
	}
	if inv.cmd.destructive {
		if err := r.confirm(inv, inv.cmd.confirm); err != nil {
			return err
		}
	}
	if inv.cmd.paginated && inv.boolean("--all") {
		return r.emitAllPages(*req)
	}
	resp, err := r.client.Do(r.ctx, *req)
	if err != nil {
		return err
	}
	r.checkEffect(inv, resp)
	return r.out.Emit(resp)
}

// request builds one call from the command's flags and positionals against
// the route inventory, so a parameter cannot be sent that the route does not
// define (spec section 7.1 refuses one anyway, and refusing it here names the
// flag rather than the wire name).
func (r *runner) request(inv *invocation, routeName string) (*Request, error) {
	rt, ok := routeByName(routeName)
	if !ok {
		return nil, &ContractError{Msg: fmt.Sprintf("no route named %q", routeName)}
	}

	supplied := map[string]string{}
	for _, f := range inv.cmd.flags {
		if f.param == "" || !inv.has(f.name) {
			continue
		}
		supplied[f.param] = inv.str(f.name)
	}

	path := rt.path
	for _, p := range rt.pathParams() {
		value, err := r.pathValue(inv, p, supplied)
		if err != nil {
			return nil, err
		}
		path = strings.ReplaceAll(path, "{"+p+"}", url.PathEscape(value))
	}

	query := url.Values{}
	body := map[string]any{}
	for k, v := range inv.cmd.implied {
		body[k] = v
	}

	for _, f := range inv.cmd.flags {
		if f.param == "" || f.where == wPath || f.where == wLocal || !inv.has(f.name) {
			continue
		}
		value, err := flagValue(inv, f)
		if err != nil {
			return nil, err
		}
		switch f.where {
		case wQuery:
			query.Set(f.param, fmt.Sprintf("%v", value))
		case wBody:
			body[f.param] = value
		}
	}

	for i, p := range inv.cmd.pos {
		if p.where == wPath || p.where == wLocal || p.param == "" {
			continue
		}
		if p.variadic {
			rest := inv.pos[min(i, len(inv.pos)):]
			if len(rest) == 0 {
				continue
			}
			if p.where == wBody {
				body[p.param] = rest
			}
			continue
		}
		v := inv.positional(i)
		if v == "" {
			continue
		}
		switch p.where {
		case wQuery:
			query.Set(p.param, v)
		case wBody:
			body[p.param] = v
		}
	}

	req := &Request{Method: rt.method, Path: path}
	if len(query) > 0 {
		req.Query = query
	}
	if len(body) > 0 {
		req.Body = body
	}
	if rt.idempotent {
		req.IdempotencyKey = r.idempotencyKey()
	}
	return req, nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// pathValue finds the value for a path parameter: a positional, a flag, or
// the --account default. A missing one is exit 2 naming what to type, never a
// request with an empty segment in the URL.
func (r *runner) pathValue(inv *invocation, param string, supplied map[string]string) (string, error) {
	for i, p := range inv.cmd.pos {
		if p.where != wPath || p.param != param {
			continue
		}
		if v := inv.positional(i); v != "" {
			return v, nil
		}
		if p.fromFlag != "" {
			if v := inv.str(p.fromFlag); v != "" {
				return v, nil
			}
		}
		if param == "account_id" {
			if v := r.env.Getenv("AGENT_GM_ACCOUNT"); v != "" {
				return v, nil
			}
		}
		return "", usageErr("`agm %s` needs %s", inv.cmd.Name(), p.name)
	}
	if v, ok := supplied[param]; ok && v != "" {
		return v, nil
	}
	if param == "account_id" {
		if v := r.env.Getenv("AGENT_GM_ACCOUNT"); v != "" {
			return v, nil
		}
		return "", flagErr("--account", "is required by `agm %s`", inv.cmd.Name())
	}
	return "", usageErr("`agm %s` needs a value for %s", inv.cmd.Name(), param)
}

// flagValue converts a flag to the JSON type the route expects.
func flagValue(inv *invocation, f flagDef) (any, error) {
	switch f.kind {
	case kBool:
		return inv.boolean(f.name), nil
	case kInt:
		n, err := inv.integer(f.name)
		if err != nil {
			return nil, err
		}
		return n, nil
	case kDuration:
		d, err := inv.duration(f.name)
		if err != nil {
			return nil, err
		}
		return d.String(), nil
	default:
		return inv.str(f.name), nil
	}
}

// idempotencyKey is --idempotency-key, or one minted for this invocation.
// Minting one per invocation is what makes re-running a send send again,
// which docs/cli.md states plainly: to retry, pass the SAME key.
func (r *runner) idempotencyKey() string {
	if r.g.idempotencyKey != "" {
		return r.g.idempotencyKey
	}
	return uuid.NewString()
}

// emitAllPages walks a listing to the end and emits one value, so `--all`
// with `--json` still puts exactly one JSON value on stdout. `cursor` never
// reaches a flag: an opaque signed cursor is not something a human types
// (spec sections 7.4, 11.3).
func (r *runner) emitAllPages(req Request) error {
	var items []json.RawMessage
	var warnings []string
	var last *Response

	for {
		resp, err := r.client.Do(r.ctx, req)
		if err != nil {
			return err
		}
		last = resp
		warnings = append(warnings, resp.Warnings...)
		page, ok := itemsOf(resp.Data)
		if !ok {
			return r.out.Emit(resp)
		}
		items = append(items, page...)
		if resp.NextCursor == nil || *resp.NextCursor == "" {
			break
		}
		if req.Query == nil {
			req.Query = url.Values{}
		}
		req.Query.Set("cursor", *resp.NextCursor)
	}

	merged, err := json.Marshal(map[string]any{"items": items})
	if err != nil {
		return &ContractError{Msg: "the merged pages could not be encoded"}
	}
	return r.out.Emit(&Response{
		Status:    last.Status,
		Data:      merged,
		Warnings:  warnings,
		RequestID: last.RequestID,
	})
}

// confirm is the destructive-command gate of spec section 11.3: the prompt
// goes to stderr so it never disturbs `--json`, and `y` or `--yes` is the
// only thing that continues.
func (r *runner) confirm(inv *invocation, effect string) error {
	if r.g.yes {
		return nil
	}
	_, _ = fmt.Fprintf(r.env.Stderr, "This %s\n", effect)
	_, _ = fmt.Fprint(r.env.Stderr, "Continue? [y/N] ")

	reader := bufio.NewReader(r.env.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil && line == "" {
		_, _ = fmt.Fprintln(r.env.Stderr)
		return ErrAborted
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return nil
	default:
		return ErrAborted
	}
}

// checkEffect compares the sentence the route returned with the one the
// prompt showed. The CLI PRINTS the route's, always (section 16 Slice 2 test
// 28); this is the local check that says so out loud when the two differ,
// which is the only case where a human confirmed different words from the
// ones the server acted on.
func (r *runner) checkEffect(inv *invocation, resp *Response) {
	if !inv.cmd.destructive || inv.cmd.confirm == "" {
		return
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(resp.Data, &obj); err != nil {
		return
	}
	served, ok := stringField(obj, "effect")
	if !ok || served == inv.cmd.confirm {
		return
	}
	r.out.Warnf("the server's effect sentence is not the one you confirmed:\n  confirmed: %s\n  served:    %s",
		inv.cmd.confirm, served)
}

// checkPositionals rejects a missing required positional and an extra one
// before anything is sent.
func checkPositionals(inv *invocation) error {
	required := 0
	variadic := false
	for _, p := range inv.cmd.pos {
		if p.required {
			required++
		}
		if p.variadic {
			variadic = true
		}
	}
	// A positional with a fromFlag is satisfied by that flag instead.
	for i, p := range inv.cmd.pos {
		if p.required && p.fromFlag != "" && inv.positional(i) == "" && inv.str(p.fromFlag) != "" {
			required--
		}
	}
	if len(inv.pos) < required {
		return usageErr("`agm %s` takes %s", inv.cmd.Name(), positionalUsage(inv.cmd))
	}
	if !variadic && len(inv.pos) > len(inv.cmd.pos) {
		return usageErr("`agm %s` takes %s, and got %d arguments",
			inv.cmd.Name(), positionalUsage(inv.cmd), len(inv.pos))
	}
	return nil
}

func positionalUsage(c *command) string {
	if len(c.pos) == 0 {
		return "no arguments"
	}
	names := make([]string, 0, len(c.pos))
	for _, p := range c.pos {
		n := p.name
		if p.variadic {
			n += "..."
		}
		names = append(names, n)
	}
	return strings.Join(names, " ")
}

func printUsage(w io.Writer) {
	_, _ = fmt.Fprintln(w, "agm -- one owner's agents, their Google Messages accounts.")
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintln(w, "Usage: agm <command> [arguments] [flags]")
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintln(w, "Commands:")
	names := make([]string, 0, len(commandTable()))
	byName := map[string]*command{}
	for _, c := range commandTable() {
		names = append(names, c.Name())
		byName[c.Name()] = c
	}
	sort.Strings(names)
	for _, n := range names {
		_, _ = fmt.Fprintf(w, "  %-34s %s\n", n, byName[n].summary)
	}
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintln(w, "Global flags:")
	for _, f := range globalDefs {
		_, _ = fmt.Fprintf(w, "  %-20s %s\n", f.name, f.help)
	}
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintln(w, "Durations are an integer and a unit of s, m, h or d: 30s, 15m, 2h, 7d.")
}

func printCommandHelp(w io.Writer, c *command) {
	_, _ = fmt.Fprintf(w, "agm %s -- %s\n\n", c.Name(), c.summary)
	// A command that takes no arguments reads "Usage: agm health [flags]".
	// Splicing positionalUsage in unconditionally gives "agm health no
	// arguments [flags]", which is the sentence an error message wants and
	// not the one a usage line does.
	if len(c.pos) == 0 {
		_, _ = fmt.Fprintf(w, "Usage: agm %s [flags]\n", c.Name())
	} else {
		_, _ = fmt.Fprintf(w, "Usage: agm %s %s [flags]\n", c.Name(), positionalUsage(c))
	}
	if len(c.flags) > 0 {
		_, _ = fmt.Fprintln(w, "\nFlags:")
		for _, f := range c.flags {
			_, _ = fmt.Fprintf(w, "  %-20s %s\n", f.name, f.help)
		}
	}
	_, _ = fmt.Fprintln(w, "\nEvery command also takes the global flags of spec 11.1.")
}
