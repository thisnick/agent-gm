package audit

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/thisnick/agent-gm/internal/clock"
)

func newTestWriter(t *testing.T) (*Writer, *MemoryAppender) {
	t.Helper()
	a := NewMemoryAppender()
	return NewWriter(a, clock.NewFake()), a
}

func ok(kind Kind) Event {
	return Event{Kind: kind, Result: ResultOK, Source: "127.0.0.1", Payload: map[string]any{"count": 1}}
}

// TestAccountShapedKindNeedsAnAccount is section 12.4: every audit row carries
// account_id where the event belongs to an account, which is how "what
// happened to this account" stays answerable after the account is removed
// (section 4.7). The writer refuses rather than storing a row nobody will
// find.
func TestAccountShapedKindNeedsAnAccount(t *testing.T) {
	ctx := context.Background()
	w, a := newTestWriter(t)

	for _, k := range []Kind{
		KindAccountPaired, KindAccountSignedOut, KindAccountRemoved,
		KindAccountStateChanged, KindOperationSend, KindOperationCrashRecovered,
		KindMessageStatusOutOfOrder,
	} {
		err := w.Append(ctx, ok(k))
		if err == nil {
			t.Errorf("%s without an account_id was accepted", k)
			continue
		}
		if !strings.Contains(err.Error(), string(k)) || !strings.Contains(err.Error(), "account_id") {
			t.Errorf("%s: refusal %q must name the kind and account_id", k, err)
		}
	}
	if n := len(a.Rows()); n != 0 {
		t.Fatalf("refused events wrote %d rows", n)
	}

	e := ok(KindAccountPaired)
	e.AccountID = "acct_5ec9ab28-5363-57f8-b543-27672479b604"
	if err := w.Append(ctx, e); err != nil {
		t.Fatalf("with an account_id: %v", err)
	}
	rows := a.Rows()
	if len(rows) != 1 || rows[0].AccountID == nil || *rows[0].AccountID != e.AccountID {
		t.Fatalf("rows = %+v", rows)
	}
}

// TestServerWideKindHasANullAccount is the other half: a settings or OAuth
// event has no account, and the column is NULL rather than the empty string,
// so `WHERE account_id = ''` cannot quietly find rows that belong to nobody.
func TestServerWideKindHasANullAccount(t *testing.T) {
	ctx := context.Background()
	w, a := newTestWriter(t)

	if err := w.Append(ctx, ok(KindSettingsChanged)); err != nil {
		t.Fatal(err)
	}
	row := a.Rows()[0]
	if row.AccountID != nil {
		t.Fatalf("account_id = %q, want NULL", *row.AccountID)
	}
	if row.AuthorizationID != nil || row.TargetType != nil || row.TargetID != nil {
		t.Errorf("unset nullable columns must be NULL, got %+v", row)
	}

	e := ok(KindSettingsChanged)
	e.AccountID = "acct_x"
	if err := w.Append(ctx, e); err == nil {
		t.Fatal("a server-wide kind with an account_id was accepted")
	}
}

// TestPairFailedMayHaveNoAccount records the one deliberate exception and why:
// a pairing that fails at GaiaInitTimeout never produced the
// AuthData.Mobile.SourceID the acct_ ID derives from (section 3.2).
func TestPairFailedMayHaveNoAccount(t *testing.T) {
	ctx := context.Background()
	w, a := newTestWriter(t)
	if err := w.Append(ctx, ok(KindAccountPairFailed)); err != nil {
		t.Fatalf("pair_failed without an account: %v", err)
	}
	e := ok(KindAccountPairFailed)
	e.AccountID = "acct_x"
	if err := w.Append(ctx, e); err != nil {
		t.Fatalf("pair_failed with an account: %v", err)
	}
	if n := len(a.Rows()); n != 2 {
		t.Fatalf("rows = %d, want 2", n)
	}
}

// TestEmptySourceIsRefused: the source field is the resolved client source of
// section 12.3, which is never the empty string.
func TestEmptySourceIsRefused(t *testing.T) {
	w, a := newTestWriter(t)
	e := ok(KindSettingsChanged)
	e.Source = ""
	err := w.Append(context.Background(), e)
	if err == nil {
		t.Fatal("an empty source was accepted")
	}
	if !strings.Contains(err.Error(), "source") {
		t.Errorf("refusal %q must name source", err)
	}
	if len(a.Rows()) != 0 {
		t.Fatal("a refused event was stored")
	}
}

func TestUndeclaredKindIsRefused(t *testing.T) {
	w, _ := newTestWriter(t)
	err := w.Append(context.Background(), ok(Kind("account.pared")))
	if err == nil || !strings.Contains(err.Error(), "account.pared") {
		t.Fatalf("a typo'd kind was accepted or not named: %v", err)
	}
}

func TestBadResultIsRefused(t *testing.T) {
	w, _ := newTestWriter(t)
	e := ok(KindSettingsChanged)
	e.Result = "succeeded"
	if err := w.Append(context.Background(), e); err == nil {
		t.Fatal("an undeclared result was accepted")
	}
}

// TestAuditIsAppendOnlyStructurally proves section 12.4's "never rewritten"
// the way it has to be proven: not by reading callers, but by showing there is
// nothing to call. The persistence interface has exactly one method, and the
// writer exposes exactly one.
func TestAuditIsAppendOnlyStructurally(t *testing.T) {
	iface := reflect.TypeOf((*Appender)(nil)).Elem()
	if iface.NumMethod() != 1 {
		var names []string
		for i := 0; i < iface.NumMethod(); i++ {
			names = append(names, iface.Method(i).Name)
		}
		t.Fatalf("Appender has methods %v; it must have exactly AppendAudit, so that no "+
			"migration and no handler can update or delete an audit row", names)
	}
	if got := iface.Method(0).Name; got != "AppendAudit" {
		t.Fatalf("Appender's only method is %q, want AppendAudit", got)
	}

	wt := reflect.TypeOf(&Writer{})
	var exported []string
	for i := 0; i < wt.NumMethod(); i++ {
		exported = append(exported, wt.Method(i).Name)
	}
	if len(exported) != 1 || exported[0] != "Append" {
		t.Fatalf("*Writer exposes %v, want exactly [Append]", exported)
	}

	// And nothing anywhere in the package offers an update or a delete.
	for _, typ := range []reflect.Type{
		reflect.TypeOf(&MemoryAppender{}),
	} {
		for i := 0; i < typ.NumMethod(); i++ {
			n := strings.ToLower(typ.Method(i).Name)
			if strings.Contains(n, "update") || strings.Contains(n, "delete") ||
				strings.Contains(n, "purge") || strings.Contains(n, "prune") {
				t.Errorf("%s has method %s; audit rows are never rewritten",
					typ, typ.Method(i).Name)
			}
		}
	}
}

func TestRowCarriesIDResultAndTimestamp(t *testing.T) {
	w, a := newTestWriter(t)
	e := ok(KindAdminBackup)
	e.Result = ResultFailed
	e.TargetType = "backup"
	e.TargetID = "bkp_1"
	e.AuthorizationID = "authz_1"
	if err := w.Append(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	row := a.Rows()[0]
	if len(row.ID) != 36 {
		t.Errorf("id %q is not a UUID", row.ID)
	}
	if row.Kind != string(KindAdminBackup) || row.Result != "failed" || row.Source != "127.0.0.1" {
		t.Errorf("row = %+v", row)
	}
	if row.CreatedAtMs != clock.NewFake().Now().UnixMilli() {
		t.Errorf("created_at_ms = %d, want the injected clock's now", row.CreatedAtMs)
	}
	if row.PayloadJSON != `{"count":1}` {
		t.Errorf("payload = %s", row.PayloadJSON)
	}
	if *row.TargetType != "backup" || *row.TargetID != "bkp_1" || *row.AuthorizationID != "authz_1" {
		t.Errorf("row = %+v", row)
	}
}

// TestEveryDeclaredKindHasARule catches a kind added to the constants and
// forgotten in the account-rule table: it would be refused at runtime by
// every caller, which is a worse failure than a compile error.
func TestEveryDeclaredKindHasARule(t *testing.T) {
	for _, k := range Kinds() {
		if _, ok := Rule(k); !ok {
			t.Errorf("%s has no account rule", k)
		}
	}
	// The kinds section 12.4 names by hand, so a rename is caught.
	for _, k := range []Kind{
		"account.paired", "account.resumed", "account.signed_out", "account.removed",
		"account.label_changed", "account.pair_failed", "account.state_changed",
		"auth.admin_session_minted", "auth.admin_session_narrowed",
		"auth.admin_session_refreshed", "auth.admin_secret_failed",
		"operation.crash_recovered", "message.status_out_of_order",
		"settings.changed", "admin.backup", "admin.backup_pruned",
		"security.unsafe_trace_enabled",
		"enrollment.created", "enrollment.consumed", "enrollment.expired",
		"enrollment.revoked",
		"authorization.created", "authorization.approved", "authorization.denied",
		"authorization.expired", "authorization.revoked", "client.revoked",
	} {
		if _, declared := Rule(k); !declared {
			t.Errorf("spec section 12.4 names %q and it is not declared", k)
		}
	}
}
