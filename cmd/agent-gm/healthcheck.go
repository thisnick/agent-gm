package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"
)

// `agent-gm healthcheck` is the container's liveness probe (spec section
// 14.1). It does a GET /healthz against this process's own listen address
// and exits 0 only on 200.
//
// It exists because the runtime image is `gcr.io/distroless/static-debian12`,
// which has no shell and no `curl`: a HEALTHCHECK there can only run a
// binary, and the only binary in the image is this one. Keeping the probe in
// the program also means it always agrees with the server about the default
// bind address -- a probe that hardcodes a port drifts the day somebody sets
// AGENT_GM_LISTEN_ADDR.
//
// It deliberately calls /healthz and not /v1/health: section 7.5's /healthz
// never touches SQLite and needs no scope, so it answers during a migration
// and without a token. /v1/health would report an unhealthy container for
// every account-level condition, which is exactly backwards (section 15.3).

// healthcheckTimeout is the default deadline for the whole probe. A liveness
// probe that hangs is worse than one that fails: Docker's own HEALTHCHECK
// timeout would kill it, but a supervisor calling the subcommand directly
// might not.
const healthcheckTimeout = 3 * time.Second

// exitUnhealthy is what a failed probe exits with. Section 11.2 leaves `1`
// unassigned and assigns `7` to a retryable condition, which is what "the
// server did not answer 200 just now" is: the caller is a supervisor whose
// entire response is to try again shortly.
const exitUnhealthy = exitRetryable

func runHealthcheck(args []string) int {
	fs := flag.NewFlagSet("healthcheck", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	addr := fs.String("addr", "", "address to probe (default $AGENT_GM_LISTEN_ADDR, else 0.0.0.0:8080)")
	timeout := fs.Duration("timeout", healthcheckTimeout, "deadline for the whole probe")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `Usage: agent-gm healthcheck [--addr <host:port>] [--timeout <duration>]

GET /healthz against this server's own listen address. Exits 0 on 200 and 7
otherwise. This is the container HEALTHCHECK: the runtime image has no shell
and no curl.

`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(os.Stderr, "agent-gm healthcheck: unexpected argument %q\n", fs.Arg(0))
		return exitUsage
	}

	raw := *addr
	if raw == "" {
		raw = os.Getenv("AGENT_GM_LISTEN_ADDR")
	}
	if raw == "" {
		// The same default internal/config.LoadServer applies, repeated here
		// rather than imported because the probe must not load, validate or
		// require any of the server's other configuration: a container whose
		// AGENT_GM_DATA_KEY is missing is unhealthy, and the probe's job is
		// to say so, not to fail for the same reason twice.
		raw = defaultListenAddr
	}

	url, err := healthcheckURL(raw)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-gm healthcheck: %v\n", err)
		return exitUsage
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	if err := probe(ctx, newHealthcheckClient(), url); err != nil {
		fmt.Fprintf(os.Stderr, "agent-gm healthcheck: %v\n", err)
		return exitUnhealthy
	}
	return exitOK
}

// defaultListenAddr mirrors AGENT_GM_LISTEN_ADDR's default (spec section
// 15.1). There is a test that it agrees with internal/config.
const defaultListenAddr = "0.0.0.0:8080"

// healthcheckURL turns a *bind* address into a *dial* URL.
//
// The two are not the same string. `0.0.0.0:8080` means "every interface" to
// a listener and is not a destination; `[::]:8080` and a bare `:8080` are the
// same wildcard. Dialling the wildcard happens to work on Linux, where the
// kernel rewrites it to loopback, and does not work everywhere -- so the
// probe rewrites it itself, to loopback of the matching family.
func healthcheckURL(bind string) (string, error) {
	host, port, err := net.SplitHostPort(bind)
	if err != nil {
		return "", fmt.Errorf("listen address %q is not host:port: %w", bind, err)
	}
	if port == "" {
		return "", fmt.Errorf("listen address %q names no port", bind)
	}
	switch host {
	case "", "0.0.0.0":
		host = "127.0.0.1"
	case "::", "[::]":
		host = "::1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/healthz", nil
}

// newHealthcheckClient does not follow redirects. A liveness probe asks one
// server one question: a 301 away from /healthz is an answer of "not the
// thing I asked about", and following it would let a misconfigured front end
// report the container healthy on the strength of somebody else's 200.
func newHealthcheckClient() *http.Client {
	return &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// probe is the request itself, split out so the test can drive it against an
// httptest server without a subprocess.
func probe(ctx context.Context, client *http.Client, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	// Drain a little so the connection can be reused and so a server that
	// wrote a body does not see a broken pipe in its log for every probe.
	_, _ = io.CopyN(io.Discard, resp.Body, 4096)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return nil
}
