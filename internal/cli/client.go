package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/thisnick/agent-gm/internal/apierr"
)

// DefaultTimeout bounds a request when --timeout is not given.
const DefaultTimeout = 60 * time.Second

// Client is `agm`'s whole knowledge of the server: a base URL, a credential
// and a timeout. It speaks the REST API and nothing else (spec section 11),
// so anything the CLI can do an agent can do too.
//
// It turns a `/v1` error envelope back into an *apierr.Error, which is what
// gives the exit-code mapping of section 11.2 something typed to work on:
// the CLI never re-derives an exit code from an HTTP status, it reads the
// code out of the envelope and hands it to apierr.ExitCodeFor.
//
// The token is never logged, never placed in a URL and never rendered by
// String(): section 10.3's rule for tickets is the same rule for the access
// token, because a proxy log and a shell history do not distinguish them.
type Client struct {
	BaseURL string
	Token   string
	Timeout time.Duration
	HTTP    *http.Client
	// UserAgent identifies the CLI in the server's logs.
	UserAgent string
}

// String describes the client without its credential.
func (c *Client) String() string {
	return fmt.Sprintf("agm client for %s (credential redacted)", c.BaseURL)
}

// Request is one call. Path is a `/v1` path with its parameters already
// substituted; Query and Body are what the route inventory says the route
// accepts, and nothing else, because every route rejects an unknown one
// (spec section 7.1).
type Request struct {
	Method string
	Path   string
	Query  url.Values
	// Body is marshalled as JSON. Nil sends no body.
	Body any
	// RawBody, when set, is sent verbatim with ContentType. It is the upload
	// PUT of section 10.2 and nothing else.
	RawBody     []byte
	ContentType string
	// IdempotencyKey sets the Idempotency-Key header (spec section 6.3). The
	// CLI uses the header transport rather than the client_request_id body
	// field so that one code path covers every mutation, including the two
	// DELETEs whose body would otherwise be empty.
	IdempotencyKey string
	// Token overrides the client's credential for this request. It carries
	// an upload or download ticket, which goes in the Authorization header
	// and never in the URL (spec section 10.3).
	Token string
	// AbsoluteURL, when set, replaces BaseURL+Path. The server hands out
	// upload_url and download_url built from AGENT_GM_PUBLIC_URL, and a
	// client that rebuilt them itself would defeat the point.
	AbsoluteURL string
	// Accept overrides the Accept header, for the SSE stream.
	Accept string
}

// Response is one answer. Data is the envelope's `data` left undecoded, so a
// command decodes only the shape it needs and `--json` can re-emit the
// envelope byte for byte.
type Response struct {
	Status     int
	Data       json.RawMessage
	NextCursor *string
	Warnings   []string
	RequestID  string
	// Raw is the whole body, for a route that serves bytes rather than an
	// envelope.
	Raw []byte
	// Header is the response header, for Content-Disposition and the like.
	Header http.Header
}

// envelope is the success envelope of spec section 7.1 as the CLI reads it.
type envelope struct {
	Data       json.RawMessage `json:"data"`
	NextCursor *string         `json:"next_cursor"`
	Warnings   []string        `json:"warnings"`
	RequestID  string          `json:"request_id"`
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

func (c *Client) urlFor(r Request) (string, error) {
	base := r.AbsoluteURL
	if base == "" {
		if c.BaseURL == "" {
			return "", &LocalError{Msg: noServerMessage}
		}
		base = strings.TrimRight(c.BaseURL, "/") + r.Path
	}
	u, err := url.Parse(base)
	if err != nil {
		return "", &LocalError{Msg: fmt.Sprintf("%q is not a URL", base), Err: err}
	}
	if len(r.Query) > 0 {
		u.RawQuery = r.Query.Encode()
	}
	return u.String(), nil
}

// build assembles the *http.Request. It is separate from Do so a test can
// inspect the headers without a server.
func (c *Client) build(ctx context.Context, r Request) (*http.Request, error) {
	target, err := c.urlFor(r)
	if err != nil {
		return nil, err
	}

	var body io.Reader
	contentType := r.ContentType
	switch {
	case r.RawBody != nil:
		body = bytes.NewReader(r.RawBody)
		if contentType == "" {
			contentType = "application/octet-stream"
		}
	case r.Body != nil:
		encoded, err := json.Marshal(r.Body)
		if err != nil {
			return nil, &LocalError{Msg: "the request body could not be encoded", Err: err}
		}
		body = bytes.NewReader(encoded)
		contentType = "application/json"
	}

	req, err := http.NewRequestWithContext(ctx, r.Method, target, body)
	if err != nil {
		return nil, &LocalError{Msg: "the request could not be built", Err: err}
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	accept := r.Accept
	if accept == "" {
		accept = "application/json"
	}
	req.Header.Set("Accept", accept)
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}
	// The credential travels in the Authorization header and nowhere else:
	// never a query parameter, never a URL (spec sections 10.3, 12.1).
	if token := r.Token; token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	} else if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	if r.IdempotencyKey != "" {
		req.Header.Set("Idempotency-Key", r.IdempotencyKey)
	}
	return req, nil
}

// Do sends one request and returns the decoded envelope, or an *apierr.Error
// built from the error envelope. A transport failure is a *TransportError,
// which is exit 7; a body that is not an envelope at all is a
// *ContractError, which is exit 10.
func (c *Client) Do(ctx context.Context, r Request) (*Response, error) {
	if c.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.Timeout)
		defer cancel()
	}

	req, err := c.build(ctx, r)
	if err != nil {
		return nil, err
	}

	resp, err := c.httpClient().Do(req)
	if err != nil {
		// The error string from net/http carries the URL, which is safe --
		// the token is not in it, by construction.
		return nil, &TransportError{Op: r.Method + " " + r.Path, Err: err}
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, &TransportError{Op: "reading the response to " + r.Method + " " + r.Path, Err: err}
	}

	out := &Response{Status: resp.StatusCode, Raw: raw, Header: resp.Header}

	if resp.StatusCode >= 400 {
		return out, errorFromEnvelope(resp.StatusCode, raw)
	}

	// 204, and the bytes routes, carry no envelope.
	if len(bytes.TrimSpace(raw)) == 0 {
		out.Data = json.RawMessage("null")
		return out, nil
	}
	if ct := resp.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(ct, "application/json") {
		return out, nil
	}

	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return out, &ContractError{Msg: fmt.Sprintf(
			"the server answered %d with a body that is not the envelope of spec 7.1: %v",
			resp.StatusCode, err)}
	}
	out.Data = env.Data
	out.NextCursor = env.NextCursor
	out.Warnings = env.Warnings
	out.RequestID = env.RequestID
	return out, nil
}

// maxResponseBytes bounds what the CLI will buffer from one response. It is
// generous -- a listing of 100 rows is far smaller -- and exists so a
// misbehaving server cannot exhaust the client's memory.
const maxResponseBytes = 64 << 20

// errorFromEnvelope turns the error envelope of spec section 7.1 into an
// *apierr.Error, so that the exit-code mapping has a typed code to look up
// rather than an HTTP status to guess from.
//
// A 4xx or 5xx whose body is not an error envelope, or whose code is not one
// of section 7.2's, is a contract failure: exit 10. That is deliberate -- an
// answer Agent GM does not recognise must not be quietly rounded to the
// nearest known code.
func errorFromEnvelope(status int, raw []byte) error {
	var body struct {
		Error struct {
			Code      apierr.Code    `json:"code"`
			Message   string         `json:"message"`
			Retryable bool           `json:"retryable"`
			Details   map[string]any `json:"details"`
		} `json:"error"`
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal(raw, &body); err != nil || body.Error.Code == "" {
		return &ContractError{Msg: fmt.Sprintf(
			"the server answered %d with a body that is not the error envelope of spec 7.1", status)}
	}
	if !apierr.Known(body.Error.Code) {
		return &ContractError{Msg: fmt.Sprintf(
			"the server answered %d with the error code %q, which is not one of spec 7.2's",
			status, body.Error.Code)}
	}
	e := &apierr.Error{
		Code:    body.Error.Code,
		Message: body.Error.Message,
		Details: body.Error.Details,
	}
	if secs := retryAfterSeconds(body.Error.Details); secs > 0 {
		e.RetryAfter = time.Duration(secs) * time.Second
	}
	return e
}

func retryAfterSeconds(details map[string]any) int {
	v, ok := details["retry_after_seconds"]
	if !ok {
		return 0
	}
	if f, ok := v.(float64); ok {
		return int(f)
	}
	return 0
}
