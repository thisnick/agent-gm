package api_test

import (
	"strings"
	"testing"

	"github.com/thisnick/agent-gm/internal/api"
	"github.com/thisnick/agent-gm/internal/apierr"
)

// R-9. `internal_error`'s message tells the caller "the request ID identifies
// it in the logs". For a whole slice that was untrue: the refusal hook was
// wired to Debug, so at the default level a 500 produced not one log line
// while the sentence promising otherwise went out to the caller.
//
// An unattributable 500 is exactly the error an operator cannot diagnose from
// outside, so it is the one that must never be silent.
//
// Plant: route internal_error back through Deps.Log and this test fails at
// "no error line was logged". Planted 2026-09-07.
func TestSlice2_R9_AnInternalErrorIsAlwaysLoggedAtErrorLevel(t *testing.T) {
	s := newServer(t)
	defer s.close()

	var refusals, errorsLogged []string
	s.Server.SetLoggers(
		func(msg string, kv ...any) { refusals = append(refusals, msg) },
		func(msg string, kv ...any) {
			line := msg
			for i := 0; i+1 < len(kv); i += 2 {
				line += " " + kv[i].(string) + "=" + toStr(kv[i+1])
			}
			errorsLogged = append(errorsLogged, line)
		},
	)

	// A handler that fails in a way nobody classified: the shape of a bug.
	if err := s.Server.Handle("healthz", func(*api.Request) (*api.Response, error) {
		return nil, errBoom{}
	}); err != nil {
		t.Fatal(err)
	}
	env := s.call("GET", "/healthz", nil)
	if env.Error == nil || env.Error.Code != string(apierr.CodeInternalError) {
		t.Fatalf("expected internal_error, got %+v", env.Error)
	}

	if len(errorsLogged) == 0 {
		t.Fatal("no error line was logged for an internal_error, though its message " +
			"tells the caller the request ID identifies it in the logs")
	}
	line := errorsLogged[0]
	if !strings.Contains(line, "request_id=req_") {
		t.Errorf("the error line carries no request_id: %q", line)
	}
	if !strings.Contains(line, "boom") {
		t.Errorf("the error line carries no cause: %q", line)
	}
	// And it did NOT go out as an ordinary refusal, which is where it was
	// getting lost.
	for _, r := range refusals {
		if strings.Contains(r, "refused") {
			t.Errorf("the internal error was also logged as an ordinary refusal: %q", r)
		}
	}
}

type errBoom struct{}

func (errBoom) Error() string { return "boom: a wrapped cause" }

func toStr(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
