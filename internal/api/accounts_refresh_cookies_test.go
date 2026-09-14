package api_test

import (
	"context"
	"testing"

	"github.com/thisnick/agent-gm/internal/gm"
	"github.com/thisnick/agent-gm/internal/gm/fake"
	"github.com/thisnick/agent-gm/internal/store"
)

// A successful cookie refresh is an account-scoped recovery, not merely a
// session-file update. This is the regression for a production refresh that
// returned success and wrote fresh cookies while GET /v1/accounts continued
// to report signed_out until the whole container was restarted.
func TestRefreshCookiesReconnectsSignedOutAccountWithoutProcessRestart(t *testing.T) {
	for _, tc := range []struct {
		name        string
		stopWorkers bool
	}{
		{name: "running workers"},
		{name: "stopped workers", stopWorkers: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newServer(t)
			defer s.close()

			ctx := context.Background()
			backend := fake.New(addressA, fake.WithClock(s.Clock))
			acct, err := s.Sup.Pair(ctx, backend, refreshTestCookies(), 0, nil)
			if err != nil {
				t.Fatalf("pairing fixture account: %v", err)
			}
			defer s.Sup.StopAll(context.Background())
			s.accounts[acct.ID] = backend

			if tc.stopWorkers {
				s.Sup.Stop(acct)
			}
			acct.Apply(ctx, &gm.EventListenFatalError{CredentialsDead: true})

			before, err := s.Store.Account(ctx, acct.ID)
			if err != nil {
				t.Fatal(err)
			}
			if before.State != store.StateSignedOut {
				t.Fatalf("fixture account is %s, want signed_out", before.State)
			}

			s.call("POST", "/v1/accounts/"+acct.ID+"/refresh-cookies", map[string]any{
				"cookies": refreshTestCookies(),
			}).ok(t, 200)

			after, err := s.Store.Account(ctx, acct.ID)
			if err != nil {
				t.Fatal(err)
			}
			if after.State != store.StateConnected || after.StateReason != "" {
				t.Errorf("account is %s/%q after refresh, want connected with no reason",
					after.State, after.StateReason)
			}
			if !acct.Running() {
				t.Error("account workers are stopped after a successful refresh")
			}
			if !backend.IsConnected() {
				t.Error("backend is disconnected after a successful refresh")
			}
			if got := backend.CallCount("RefreshGoogleCookies"); got != 1 {
				t.Errorf("RefreshGoogleCookies called %d times, want 1", got)
			}
			if got := backend.CallCount("Connect"); got != 2 {
				t.Errorf("Connect called %d times, want 2 (initial pair and in-process recovery)", got)
			}
		})
	}
}

func refreshTestCookies() map[string]string {
	return map[string]string{
		"SID": "fixture", "HSID": "fixture", "OSID": "fixture",
		"SSID": "fixture", "APISID": "fixture", "SAPISID": "fixture",
		"__Secure-1PSIDTS": "fixture",
	}
}
