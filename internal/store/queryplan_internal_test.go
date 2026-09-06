package store

// Spec section 16 Slice 2 test 39: every advertised listing is indexed in
// BOTH forms. EXPLAIN QUERY PLAN runs over each list and search route, with
// and without account_id, and over every filter combination the route
// advertises; a route added without an index fails here rather than in
// production on somebody's forty-thousand-message history.
//
// The test is in-package so it can plan the exact statement the route runs,
// rather than a hand-written approximation that could drift from it.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/thisnick/agent-gm/internal/clock"
)

// scanForbidden are the tables spec section 16 test 39 names: none of them
// may be reached by a bare SCAN. A `SCAN <table> USING INDEX <name>` is an
// ordered index walk and is what the `_all` indexes of section 4.2 exist to
// provide, so it passes; a `SCAN <table>` with no index does not.
var scanForbidden = []string{"conversations", "messages", "participants", "contacts"}

func planFor(t *testing.T, st *Store, sqlText string, args []any) []string {
	t.Helper()
	rows, err := st.read.QueryContext(context.Background(), "EXPLAIN QUERY PLAN "+sqlText, args...)
	if err != nil {
		t.Fatalf("planning: %v\n%s", err, sqlText)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatalf("scanning plan: %v", err)
		}
		out = append(out, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading plan: %v", err)
	}
	return out
}

// planProblems reports what is wrong with a query plan, as a pure function so
// that the checker itself can be tested (see TestThePlanCheckerCatchesAScan).
func planProblems(plan []string) []string {
	if len(plan) == 0 {
		return []string{"empty query plan"}
	}
	var problems []string
	usedAnIndex := false
	for _, line := range plan {
		if strings.Contains(line, "USING INDEX") ||
			strings.Contains(line, "USING COVERING INDEX") ||
			strings.Contains(line, "USING INTEGER PRIMARY KEY") ||
			strings.Contains(line, "USING PRIMARY KEY") ||
			strings.Contains(line, "VIRTUAL TABLE INDEX") {
			usedAnIndex = true
		}
		if !strings.HasPrefix(line, "SCAN ") {
			continue
		}
		if strings.Contains(line, "USING") {
			continue // an ordered index walk, which is what the _all indexes give
		}
		rest := strings.TrimPrefix(line, "SCAN ")
		table := rest
		if i := strings.IndexByte(rest, ' '); i >= 0 {
			table = rest[:i]
		}
		for _, forbidden := range scanForbidden {
			if table == forbidden {
				problems = append(problems, "full table scan of "+forbidden)
			}
		}
	}
	if !usedAnIndex {
		problems = append(problems, "the plan uses no index at all")
	}
	return problems
}

// assertIndexed fails when any line of the plan reaches one of the forbidden
// tables without an index, and when the plan uses no index at all.
func assertIndexed(t *testing.T, name string, plan []string) {
	t.Helper()
	for _, p := range planProblems(plan) {
		t.Errorf("%s: %s\n  plan: %s", name, p, strings.Join(plan, "\n        "))
	}
}

// The checker has to be able to fail, or the table above proves nothing.
func TestThePlanCheckerCatchesAScan(t *testing.T) {
	st := planStore(t)
	// send_mode_raw is deliberately unindexed: it is an internal column that
	// is never served and never filtered on by a route.
	plan := planFor(t, st,
		`SELECT id FROM conversations WHERE send_mode_raw = ?`, []any{"SEND_MODE_XMS"})
	problems := planProblems(plan)
	if len(problems) == 0 {
		t.Fatalf("an unindexed query passed the checker; plan was: %v", plan)
	}
	if !strings.Contains(strings.Join(problems, "; "), "full table scan of conversations") {
		t.Errorf("problems = %v, want a conversations scan", problems)
	}
}

func planStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(t.TempDir(), clock.NewFake())
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	// The planner reads sqlite_stat1 when it exists; with an empty database
	// and no ANALYZE it plans purely from the declared indexes, which is what
	// this test is about.
	return st
}

func boolPtr(b bool) *bool { return &b }

// TestEveryAdvertisedRouteIsIndexedInBothForms is table-driven over the
// parameter sets of spec section 7.6, so a route or a filter added without an
// index fails.
func TestEveryAdvertisedRouteIsIndexedInBothForms(t *testing.T) {
	st := planStore(t)
	const acct = "acct_00000000-0000-7000-8000-000000000001"
	cur := &Cursor{SentAtMS: 1700000000000, ID: "conv_zzz"}

	// GET /v1/conversations, every advertised filter, alone and combined.
	conversationCases := map[string]ConversationQuery{
		"bare":            {},
		"query":           {Query: "alex"},
		"participant":     {ParticipantPhone: "+12025550123"},
		"participant_id":  {ParticipantID: "part_00000000-0000-7000-8000-000000000002"},
		"folder":          {Folder: "archived"},
		"type":            {Type: "rcs"},
		"unread_only":     {UnreadOnly: true},
		"group_only":      {GroupOnly: true},
		"include_deleted": {IncludeDeleted: true},
		"cursor":          {Cursor: cur},
		"everything": {Query: "alex", ParticipantPhone: "+12025550123", Folder: "active",
			Type: "rcs", UnreadOnly: true, GroupOnly: true, IncludeDeleted: true, Cursor: cur},
	}
	for name, q := range conversationCases {
		for _, withAccount := range []bool{false, true} {
			qq := q
			qq.AllAccounts = true
			if withAccount {
				qq.AccountID = acct
			}
			sqlText, args := qq.sql()
			assertIndexed(t, label(EndpointConversations, name, withAccount),
				planFor(t, st, sqlText, args))
		}
	}

	// GET /v1/messages and GET /v1/conversations/{id}/messages.
	messageCases := map[string]MessageQuery{
		"bare":             {},
		"conversation":     {ConversationID: "conv_00000000-0000-7000-8000-000000000003"},
		"direction":        {Direction: "outgoing"},
		"sender_id":        {SenderParticipantID: "part_00000000-0000-7000-8000-000000000002"},
		"sender_me":        {SenderMe: true},
		"sender_phone":     {SenderPhone: "+12025550123"},
		"after":            {AfterMS: 1700000000000},
		"before":           {BeforeMS: 1800000000000},
		"has_attachment":   {HasAttachment: boolPtr(true)},
		"no_attachment":    {HasAttachment: boolPtr(false)},
		"delivery_state":   {DeliveryState: "delivered"},
		"include_system":   {IncludeSystem: true},
		"cursor":           {Cursor: &Cursor{SentAtMS: 1700000000000, ID: "msg_zzz"}},
		"everything_me":    {SenderMe: true, Direction: "outgoing", AfterMS: 1, BeforeMS: 2, HasAttachment: boolPtr(true), DeliveryState: "read", IncludeSystem: true},
		"everything_phone": {SenderPhone: "+12025550123", ConversationID: "conv_x", AfterMS: 1, BeforeMS: 2, DeliveryState: "sent"},
	}
	for name, q := range messageCases {
		for _, withAccount := range []bool{false, true} {
			qq := q
			qq.AllAccounts = true
			if withAccount {
				qq.AccountID = acct
			}
			sqlText, args := qq.sql()
			assertIndexed(t, label(EndpointMessages, name, withAccount),
				planFor(t, st, sqlText, args))
		}
	}

	// GET /v1/messages/{id}/context.
	for name, tpl := range map[string]string{"before": contextBeforeSQL, "after": contextAfterSQL} {
		assertIndexed(t, "messages.context/"+name,
			planFor(t, st, contextSQL(tpl),
				[]any{"conv_x", int64(1), int64(1), "msg_x", false, 5}))
	}

	// GET /v1/search/messages, both modes.
	searchCases := map[string]SearchQuery{
		"words":          {Q: "marmalade", Mode: SearchModeWords},
		"exact":          {Q: "marmalade sandwich", Mode: SearchModeExact},
		"conversation":   {Q: "x", ConversationID: "conv_x"},
		"sender_me":      {Q: "x", SenderMe: true},
		"sender_phone":   {Q: "x", SenderPhone: "+12025550123"},
		"has_attachment": {Q: "x", HasAttachment: boolPtr(true)},
		"window":         {Q: "x", AfterMS: 1, BeforeMS: 2},
		"cursor":         {Q: "x", Cursor: &Cursor{SentAtMS: 1, ID: "msg_z"}},
	}
	for name, q := range searchCases {
		for _, withAccount := range []bool{false, true} {
			qq := q
			qq.AllAccounts = true
			if withAccount {
				qq.AccountID = acct
			}
			sqlText, args, ok := qq.sql()
			if !ok {
				t.Fatalf("search case %s produced no query", name)
			}
			assertIndexed(t, label(EndpointSearch, name, withAccount),
				planFor(t, st, sqlText, args))
		}
	}

	// GET /v1/contacts.
	contactCases := map[string]ContactQuery{
		"bare":   {},
		"query":  {Query: "alex"},
		"top":    {Top: true},
		"cursor": {Cursor: &Cursor{SentAtMS: 1, ID: "contact_z"}},
		"all":    {Query: "alex", Top: true, Cursor: &Cursor{SentAtMS: 1, ID: "contact_z"}},
	}
	for name, q := range contactCases {
		for _, withAccount := range []bool{false, true} {
			qq := q
			qq.AllAccounts = true
			if withAccount {
				qq.AccountID = acct
			}
			sqlText, args := qq.sql()
			assertIndexed(t, label(EndpointContacts, name, withAccount),
				planFor(t, st, sqlText, args))
		}
	}

	// GET /v1/operations.
	terminal := true
	operationCases := map[string]OperationQuery{
		"bare":     {AuthorizationID: "auth_1"},
		"kind":     {AuthorizationID: "auth_1", Kind: "send_text"},
		"status":   {AuthorizationID: "auth_1", Status: OpPending},
		"terminal": {AuthorizationID: "auth_1", Terminal: &terminal},
		"window":   {AuthorizationID: "auth_1", AfterMS: 1, BeforeMS: 2},
		"cursor":   {AuthorizationID: "auth_1", Cursor: &Cursor{SentAtMS: 1, ID: "op_z"}},
	}
	for name, q := range operationCases {
		for _, withAccount := range []bool{false, true} {
			qq := q
			qq.AllAccounts = true
			if withAccount {
				qq.AccountID = acct
			}
			sqlText, args := qq.sql()
			assertIndexed(t, label(EndpointOperations, name, withAccount),
				planFor(t, st, sqlText, args))
		}
	}

	// GET /v1/admin/audit.
	auditCases := map[string]AuditQuery{
		"kind":        {Kind: "account.removed"},
		"kind_prefix": {KindPrefix: "account."},
		"account":     {AccountID: acct},
		"auth":        {AuthorizationID: "auth_1"},
		"window":      {Kind: "account.removed", AfterMS: 1, BeforeMS: 2},
		"cursor":      {Kind: "account.removed", Cursor: &Cursor{SentAtMS: 1, ID: "z"}},
	}
	for name, q := range auditCases {
		sqlText, args := q.sql()
		assertIndexed(t, EndpointAudit+"/"+name, planFor(t, st, sqlText, args))
	}
}

func label(endpoint, name string, withAccount bool) string {
	form := "all-accounts"
	if withAccount {
		form = "account_id"
	}
	return fmt.Sprintf("%s/%s [%s]", endpoint, name, form)
}
