package authz_test

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
	"time"

	"github.com/thisnick/agent-gm/internal/authz"
	"github.com/thisnick/agent-gm/internal/clock"
	"github.com/thisnick/agent-gm/internal/store"
)

// Spec section 16 Slice 3 test 11, the durable half: thirty invalid presented
// tokens in 15 minutes trip a cooldown that SURVIVES A RESTART; a successful
// presentation does not clear the counter; and the presented token is looked
// up read-only before any write transaction opens.
//
// It lives here rather than in the HTTP suite because the in-memory bucket of
// section 9.8 (60 a minute, burst 20) refuses a caller long before thirty
// guesses are possible, and waiting a minute out is not something a test may
// do (section 13.1). Here the clock is injected and the "restart" is real: the
// store is closed and reopened over the same file.

func newBudgetService(t *testing.T, dir string, clk clock.Clock) (*store.Store, *authz.Service) {
	t.Helper()
	st, err := store.Open(dir, clk)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := authz.New(st, clk, authz.NewMemorySettings(), nil, authz.Config{
		AdminSecret: "a-test-admin-secret-well-over-the-43-character-minimum-0123456789",
		PublicURL:   "https://gm.agent-wx.app",
	})
	if err != nil {
		t.Fatal(err)
	}
	return st, svc
}

// TestDurableTokenBudgetSurvivesARestart is the durability claim itself.
//
// Plant: change LimitOAuthToken's Kind to something `oauth_attempts` never
// stores, or move the counter into a map on the Service, and this fails at
// "after reopening the store".
func TestDurableTokenBudgetSurvivesARestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	clk := clock.NewFake()

	st, svc := newBudgetService(t, dir, clk)

	// Thirty failures. The thirtieth sets the cooldown.
	for i := 0; i < 30; i++ {
		if _, err := svc.Durable.RecordFailure(ctx, authz.LimitOAuthToken, "203.0.113.7"); err != nil {
			t.Fatalf("recording failure %d: %v", i, err)
		}
	}
	if err := svc.Durable.Check(ctx, authz.LimitOAuthToken, "203.0.113.7"); err == nil {
		t.Fatal("thirty invalid presented tokens did not trip the cooldown")
	}

	// A successful presentation does not clear the counter: there is no
	// RecordSuccess to call, and the row is untouched by one. This asserts
	// the absence, which is the thing section 9.8 actually requires.
	before, ok, err := st.AttemptRow(ctx, store.AttemptKindOAuthToken, "203.0.113.7")
	if err != nil || !ok {
		t.Fatalf("reading the attempt row: %v (found=%v)", err, ok)
	}

	// Close and reopen: a real restart of the only thing that holds this
	// counter.
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st2, svc2 := newBudgetService(t, dir, clk)
	t.Cleanup(func() { _ = st2.Close() })

	if err := svc2.Durable.Check(ctx, authz.LimitOAuthToken, "203.0.113.7"); err == nil {
		t.Fatal("after reopening the store the cooldown was gone. The limit lives in " +
			"oauth_attempts precisely because it is the one an attacker would " +
			"restart-cycle to reset")
	}
	after, ok, err := st2.AttemptRow(ctx, store.AttemptKindOAuthToken, "203.0.113.7")
	if err != nil || !ok {
		t.Fatalf("reading the attempt row after the restart: %v (found=%v)", err, ok)
	}
	if after.Failures != before.Failures || after.CooldownUntilMS != before.CooldownUntilMS {
		t.Errorf("the restart changed the counter: %d/%d before, %d/%d after",
			before.Failures, before.CooldownUntilMS, after.Failures, after.CooldownUntilMS)
	}

	// A different source is unaffected: the budget is per source.
	if err := svc2.Durable.Check(ctx, authz.LimitOAuthToken, "198.51.100.4"); err != nil {
		t.Errorf("an unrelated source was refused: %v", err)
	}

	// And the cooldown ends on the injected clock rather than on a wall
	// clock a test would have to wait out.
	clk.Advance(25 * time.Hour)
	if err := svc2.Durable.Check(ctx, authz.LimitOAuthToken, "203.0.113.7"); err != nil {
		t.Errorf("the cooldown outlived its own deadline: %v", err)
	}
}

// TestThePresentedValueIsLookedUpBeforeAnyWriteTransaction is section 9.8's
// ordering rule: "The presented token is looked up read-only BEFORE any write
// transaction opens, so a caller presenting a value that belongs to nobody
// cannot take the single writer's lock and queue every other writer behind
// itself."
//
// It is asserted by parsing this package's own source, because the rule is
// about ORDER and a behavioural test would have to observe the writer
// goroutine's queue -- which is exactly the sort of test that passes for the
// wrong reason. The read-only accessors are on *store.Store; the write
// transaction is st.AuthzTx. So the claim is: in each of these functions, the
// first call to a read-only accessor precedes the first call to AuthzTx.
func TestThePresentedValueIsLookedUpBeforeAnyWriteTransaction(t *testing.T) {
	fset := token.NewFileSet()
	for _, file := range []string{"oauth.go", "service.go"} {
		parsed, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", file, err)
		}
		for _, decl := range parsed.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			switch fn.Name.Name {
			case "ExchangeAuthorizationCode", "refreshSession", "RevokeToken":
			default:
				continue
			}
			readAt, writeAt := -1, -1
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				pos := fset.Position(call.Pos()).Offset
				switch sel.Sel.Name {
				case "AuthorizationCodeByHash", "TokenByHash", "Authorization":
					if readAt == -1 {
						readAt = pos
					}
				case "AuthzTx":
					if writeAt == -1 {
						writeAt = pos
					}
				}
				return true
			})
			if readAt == -1 {
				t.Errorf("%s.%s makes no read-only lookup of the presented value",
					file, fn.Name.Name)
				continue
			}
			if writeAt != -1 && writeAt < readAt {
				t.Errorf("%s.%s opens a write transaction at offset %d, before the "+
					"read-only lookup at offset %d. Section 9.8 requires the lookup "+
					"first, so a caller presenting a value that belongs to nobody "+
					"cannot take the single writer's lock",
					file, fn.Name.Name, writeAt, readAt)
			}
		}
	}
}

// TestTheEnrollmentBudgetRefusesTheEleventhAttempt pins the arithmetic of
// section 9.4's "eleventh failed code attempt within 15 minutes is 429".
//
// The off-by-one here is worth a test of its own: the limiter records a
// failure and refuses the NEXT request while the cooldown runs, so PerSource
// has to be 10 for the eleventh attempt to be the refused one. A limit of 11
// gives an attacker exactly one more free guess, and no other test would
// notice.
func TestTheEnrollmentBudgetRefusesTheEleventhAttempt(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake()
	st, svc := newBudgetService(t, t.TempDir(), clk)
	t.Cleanup(func() { _ = st.Close() })

	for i := 1; i <= 10; i++ {
		if err := svc.Durable.Check(ctx, authz.LimitEnrollmentSource, "203.0.113.7"); err != nil {
			t.Fatalf("attempt %d was refused before the eleventh: %v", i, err)
		}
		if _, err := svc.Durable.RecordFailure(ctx, authz.LimitEnrollmentSource, "203.0.113.7"); err != nil {
			t.Fatal(err)
		}
	}
	err := svc.Durable.Check(ctx, authz.LimitEnrollmentSource, "203.0.113.7")
	if err == nil {
		t.Fatal("the eleventh attempt was allowed; section 9.4 makes it the 429")
	}
	if !strings.Contains(err.Error(), "rate_limited") {
		t.Errorf("the eleventh attempt was refused with %v, want a rate limit", err)
	}
}
