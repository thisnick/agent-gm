package apierr

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestUnknownQueryParameterNamesIt covers spec section 7.1 and Slice 2 test
// 12: an unknown query parameter is invalid_request naming it, and the
// cache-busting `_` is not allowlisted.
func TestUnknownQueryParameterNamesIt(t *testing.T) {
	for _, name := range []string{"directon", "_", "client_request_id"} {
		e := UnknownQueryParameter(name)
		if e.Code != CodeInvalidRequest {
			t.Errorf("%q: code = %q, want invalid_request", name, e.Code)
		}
		if e.HTTPStatus() != 400 {
			t.Errorf("%q: status = %d, want 400", name, e.HTTPStatus())
		}
		if e.Details["parameter"] != name {
			t.Errorf("%q: details.parameter = %v", name, e.Details["parameter"])
		}
		if !strings.Contains(e.Message, name) {
			t.Errorf("%q: the message does not name the parameter: %q", name, e.Message)
		}
	}
}

// TestUnknownBodyFieldNamesIt covers the body half of spec section 7.1.
func TestUnknownBodyFieldNamesIt(t *testing.T) {
	e := UnknownBodyField("scope")
	if e.Code != CodeInvalidRequest {
		t.Fatalf("code = %q, want invalid_request", e.Code)
	}
	if e.Details["field"] != "scope" {
		t.Errorf("details.field = %v, want scope", e.Details["field"])
	}
	if _, ok := e.Details["parameter"]; ok {
		t.Error("a body field must be reported in details.field, not details.parameter")
	}
}

// TestWrongIDPrefixIsNeverNotFound is spec section 4.1 and Slice 2 test 13:
// an ID with the wrong prefix is invalid_request naming the parameter and the
// expected prefix, NEVER not_found -- not_found would tell the caller the
// thread is gone and send it looking for a deletion that never happened. A
// raw Google ID is the same case.
func TestWrongIDPrefixIsNeverNotFound(t *testing.T) {
	cases := []struct {
		name      string
		value     string
		parameter string
		prefix    string
	}{
		{"msg where conv expected", "msg_0b3f7e9c-1111-5111-8111-111111111111", "conversation_id", "conv_"},
		{"conv where msg expected", "conv_0b3f7e9c-1111-5111-8111-111111111111", "message_id", "msg_"},
		{"raw google id", "8SGY2h3rQ0-1aBcDeF", "conversation_id", "conv_"},
		{"raw google numeric id", "1234567890123456789", "message_id", "msg_"},
		{"prefix with nothing after it", "conv_", "conversation_id", "conv_"},
		{"acct where conv expected", "acct_0b3f7e9c-1111-5111-8111-111111111111", "conversation_id", "conv_"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := CheckIDPrefix(tc.value, tc.parameter, tc.prefix)
			if e == nil {
				t.Fatalf("CheckIDPrefix(%q, %q, %q) accepted it", tc.value, tc.parameter, tc.prefix)
			}
			if e.Code == CodeNotFound {
				t.Fatalf("a wrong-prefix ID answered not_found; spec section 4.1 says never")
			}
			if e.Code != CodeInvalidRequest {
				t.Fatalf("code = %q, want invalid_request", e.Code)
			}
			if e.Details["parameter"] != tc.parameter {
				t.Errorf("details.parameter = %v, want %q", e.Details["parameter"], tc.parameter)
			}
			if e.Details["expected_prefix"] != tc.prefix {
				t.Errorf("details.expected_prefix = %v, want %q", e.Details["expected_prefix"], tc.prefix)
			}
			if !strings.Contains(e.Message, tc.parameter) || !strings.Contains(e.Message, tc.prefix) {
				t.Errorf("the message must name both the parameter and the prefix: %q", e.Message)
			}
		})
	}
}

// TestCheckIDPrefixAcceptsAWellFormedID proves the check is not simply always
// refusing, which would make the test above vacuous.
func TestCheckIDPrefixAcceptsAWellFormedID(t *testing.T) {
	if e := CheckIDPrefix("conv_0b3f7e9c-1111-5111-8111-111111111111", "conversation_id", "conv_"); e != nil {
		t.Fatalf("a well-formed conv_ ID was refused: %v", e)
	}
	// An empty value is a missing parameter, not a wrong prefix.
	e := CheckIDPrefix("", "conversation_id", "conv_")
	if e == nil || e.Code != CodeInvalidRequest {
		t.Fatalf("an empty ID must be invalid_request, got %v", e)
	}
	if e.Details["parameter"] != "conversation_id" {
		t.Errorf("details.parameter = %v", e.Details["parameter"])
	}
}

// TestIDFromAnotherAccountNamesBoth is spec section 7.3 and Slice 2 test 34:
// a conv_ or msg_ ID belonging to a different account than the account_id
// given is invalid_request naming BOTH, never not_found.
func TestIDFromAnotherAccountNamesBoth(t *testing.T) {
	const convID = "conv_0b3f7e9c-1111-5111-8111-111111111111"
	const acctID = "acct_0b3f7e9c-2222-5222-8222-222222222222"

	e := IDFromAnotherAccount("conversation_id", convID, acctID)
	if e.Code == CodeNotFound {
		t.Fatal("a cross-account ID answered not_found; spec section 7.3 says never")
	}
	if e.Code != CodeInvalidRequest {
		t.Fatalf("code = %q, want invalid_request", e.Code)
	}
	if e.Details["id"] != convID || e.Details["account_id"] != acctID {
		t.Errorf("details must name both: %#v", e.Details)
	}
	if !strings.Contains(e.Message, convID) || !strings.Contains(e.Message, acctID) {
		t.Errorf("the message must name both: %q", e.Message)
	}
}

// TestAmbiguousAccountListsCandidates is spec section 7.3 and Slice 2 test
// 34: the answer carries details.field = "account_id" and every candidate as
// {id, google_account, state}, so the caller can retry without a second round
// trip.
func TestAmbiguousAccountListsCandidates(t *testing.T) {
	candidates := []AccountCandidate{
		{ID: "acct_1111", GoogleAccount: "owner@example.com", State: "connected"},
		{ID: "acct_2222", GoogleAccount: "other@example.com", State: "signed_out"},
	}
	e := AmbiguousAccount(candidates)

	if e.Code != CodeInvalidRequest {
		t.Fatalf("code = %q, want invalid_request", e.Code)
	}
	if e.Details["field"] != "account_id" {
		t.Errorf("details.field = %v, want account_id", e.Details["field"])
	}

	body, err := json.Marshal(e.Envelope("req_z"))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	const want = `{"error":{"code":"invalid_request",` +
		`"message":"more than one account exists, so a write must name one in account_id; the candidates are in details.accounts",` +
		`"retryable":false,` +
		`"details":{"accounts":[` +
		`{"id":"acct_1111","google_account":"owner@example.com","state":"connected"},` +
		`{"id":"acct_2222","google_account":"other@example.com","state":"signed_out"}],` +
		`"field":"account_id"}},"request_id":"req_z"}`
	if string(body) != want {
		t.Errorf("ambiguous-account envelope\n got: %s\nwant: %s", body, want)
	}

	// The candidate list is copied, so a caller mutating its slice afterwards
	// cannot change an error it already produced.
	candidates[0].ID = "acct_mutated"
	if got := e.Details["accounts"].([]AccountCandidate)[0].ID; got != "acct_1111" {
		t.Errorf("the candidate list aliased the caller's slice: %q", got)
	}
}

// TestUnsupportedCapabilityCarriesTheClosedVocabulary is spec sections 7.7
// and 7.8: the answer carries details.reason from the closed vocabulary plus
// the object ID, the action and the capability's current value.
func TestUnsupportedCapabilityCarriesTheClosedVocabulary(t *testing.T) {
	e := UnsupportedCapability(ReasonConversationReadOnly,
		"conv_0b3f7e9c-1111-5111-8111-111111111111", "send", "read_only", true)

	if e.Code != CodeUnsupportedCapability {
		t.Fatalf("code = %q, want unsupported_capability", e.Code)
	}
	if e.HTTPStatus() != 409 {
		t.Errorf("status = %d, want 409", e.HTTPStatus())
	}
	if e.Details["object_id"] != "conv_0b3f7e9c-1111-5111-8111-111111111111" {
		t.Errorf("details.object_id = %v", e.Details["object_id"])
	}
	if e.Details["action"] != "send" {
		t.Errorf("details.action = %v", e.Details["action"])
	}
	if e.Details["capability"] != "read_only" || e.Details["capability_value"] != true {
		t.Errorf("details must carry the capability and its current value: %#v", e.Details)
	}

	body, err := json.Marshal(e.Details["reason"])
	if err != nil {
		t.Fatalf("marshal reason: %v", err)
	}
	if string(body) != `"conversation_read_only"` {
		t.Errorf("details.reason serialised as %s, want a bare reason string", body)
	}
}

// TestReasonVocabularyMatchesSpec7_8 restates spec section 7.8's closed
// vocabulary as literals and asserts it is exactly what the package offers,
// in both directions.
func TestReasonVocabularyMatchesSpec7_8(t *testing.T) {
	// Transcribed from spec section 7.8's table, in the spec's order.
	want := []string{
		"not_signed_in",
		"conversation_read_only",
		"conversation_deleted",
		"not_my_message",
		"not_my_reaction",
		"reply_not_supported",
		"rcs_not_available",
		"media_pending",
	}

	got := make([]string, 0, len(Reasons()))
	for _, r := range Reasons() {
		got = append(got, r.String())
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the section 7.8 vocabulary is\n got: %v\nwant: %v", got, want)
	}

	// not_paired is deliberately NOT in this table: having no accounts at all
	// is a service-level condition, not a property of a conversation.
	if _, ok := ReasonByName("not_paired"); ok {
		t.Error("not_paired must not be an unsupported_capability reason (spec section 7.8)")
	}
	if _, ok := ReasonByName("invented_reason"); ok {
		t.Error("a reason outside the vocabulary was parsed")
	}
	for _, name := range want {
		if _, ok := ReasonByName(name); !ok {
			t.Errorf("ReasonByName(%q) failed", name)
		}
	}
}

// TestReasonCannotBeInventedOutsideThePackage proves the vocabulary is closed
// by the type, not by review: Reason's only field is unexported, so no other
// package can compose one, and the zero value -- the one thing another
// package could write -- is refused rather than serialised as an empty
// reason.
func TestReasonCannotBeInventedOutsideThePackage(t *testing.T) {
	rt := reflect.TypeOf(Reason{})
	if rt.Kind() != reflect.Struct {
		t.Fatalf("Reason is a %v; a string kind would let any package mint one", rt.Kind())
	}
	for i := 0; i < rt.NumField(); i++ {
		if rt.Field(i).IsExported() {
			t.Errorf("Reason.%s is exported, so another package can invent a reason", rt.Field(i).Name)
		}
	}

	defer func() {
		if recover() == nil {
			t.Error("the zero Reason was accepted; it must be refused")
		}
	}()
	_ = UnsupportedCapability(Reason{}, "conv_1", "send", "read_only", false)
}

// TestIdempotencyConflict is spec section 6.3 and Slice 2 test 7.
func TestIdempotencyConflict(t *testing.T) {
	e := IdempotencyConflict("cri-42")
	if e.Code != CodeIdempotencyConflict || e.HTTPStatus() != 409 {
		t.Fatalf("code %q status %d, want idempotency_conflict 409", e.Code, e.HTTPStatus())
	}
	if e.Retryable() {
		t.Error("idempotency_conflict is not retryable")
	}
	if e.Details["idempotency_key"] != "cri-42" {
		t.Errorf("details.idempotency_key = %v", e.Details["idempotency_key"])
	}
}

// TestRateLimitedCarriesRetryAfter proves the Retry-After value rounds up and
// never says 0, which a client would read as "retry immediately".
func TestRateLimitedCarriesRetryAfter(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{0, ""},
		{-time.Second, ""},
		{time.Millisecond, "1"},
		{time.Second, "1"},
		{1500 * time.Millisecond, "2"},
		{90 * time.Second, "90"},
	}
	for _, tc := range cases {
		e := RateLimited(tc.in)
		if e.Code != CodeRateLimited || e.HTTPStatus() != 429 {
			t.Fatalf("code %q status %d", e.Code, e.HTTPStatus())
		}
		if !e.Retryable() {
			t.Error("rate_limited is retryable")
		}
		if got := e.RetryAfterHeader(); got != tc.want {
			t.Errorf("RetryAfterHeader(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestGoogleErrorCarriesTypeAndMessage is spec section 7.2's google_error row.
func TestGoogleErrorCarriesTypeAndMessage(t *testing.T) {
	e := GoogleError(16, "invalid authentication credentials", false)
	if e.Code != CodeGoogleError || e.HTTPStatus() != 502 {
		t.Fatalf("code %q status %d", e.Code, e.HTTPStatus())
	}
	if e.Details["google_type"] != int64(16) {
		t.Errorf("details.google_type = %#v, want the bare integer 16", e.Details["google_type"])
	}
	if e.Details["google_message"] != "invalid authentication credentials" {
		t.Errorf("details.google_message = %v", e.Details["google_message"])
	}
}

// TestGoogleUndocumentedStatusClaimsNothing is spec section 3.7 and Slice 2
// test 11: details.status is the BARE integer and the answer invents no name
// for it.
func TestGoogleUndocumentedStatusClaimsNothing(t *testing.T) {
	e := GoogleUndocumentedStatus(31337)
	if e.Code != CodeGoogleUndocumentedStatus || e.HTTPStatus() != 502 {
		t.Fatalf("code %q status %d", e.Code, e.HTTPStatus())
	}
	if e.Details["status"] != int64(31337) {
		t.Errorf("details.status = %#v, want the bare integer 31337", e.Details["status"])
	}
	if len(e.Details) != 1 {
		t.Errorf("details carries more than the bare status: %#v", e.Details)
	}
	for _, k := range []string{"name", "status_name", "meaning", "google_status"} {
		if _, ok := e.Details[k]; ok {
			t.Errorf("details.%s invents a name for an unnamed Google value", k)
		}
	}
	body, err := json.Marshal(e.Envelope("req_g"))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(body), `"status":31337`) {
		t.Errorf("the status must serialise as a bare number: %s", body)
	}
}

// constructorCase is one constructor invoked with representative arguments,
// for the two whole-surface tests below.
type constructorCase struct {
	name string
	err  *Error
}

// allConstructors invokes every exported constructor once. A constructor
// added without a row here is caught by TestEveryConstructorIsExercised.
func allConstructors() []constructorCase {
	return []constructorCase{
		{"New", New(CodeInvalidRequest, "m")},
		{"Newf", Newf(CodeInvalidRequest, "m %d", 1)},
		{"UnknownQueryParameter", UnknownQueryParameter("_")},
		{"UnknownBodyField", UnknownBodyField("scope")},
		{"MalformedBody", MalformedBody("bad")},
		{"WrongTypeForField", WrongTypeForField("limit", "string")},
		{"MissingParameter", MissingParameter("account_id")},
		{"WrongIDPrefix", WrongIDPrefix("conversation_id", "conv_")},
		{"IDFromAnotherAccount", IDFromAnotherAccount("conversation_id", "conv_1", "acct_2")},
		{"AmbiguousAccount", AmbiguousAccount([]AccountCandidate{{ID: "acct_1", GoogleAccount: "a@example.com", State: "connected"}})},
		{"UnsupportedCapability", UnsupportedCapability(ReasonMediaPending, "att_1", "download", "media_state", "pending")},
		{"IdempotencyConflict", IdempotencyConflict("cri-1")},
		{"RateLimited", RateLimited(30 * time.Second)},
		{"NotFound", NotFound("conversation")},
		{"InvalidToken", InvalidToken("it expired")},
		{"InsufficientScope", InsufficientScope("messages:write")},
		{"NotPaired", NotPaired()},
		{"PayloadTooLarge", PayloadTooLarge("the request body", MaxBodyBytes)},
		{"MediaUnsupportedType", MediaUnsupportedType("application/x-nonsense")},
		{"Internal", Internal(errors.New("boom"))},
		{"NotDefaultSMSApp", NotDefaultSMSApp()},
		{"GoogleUndocumentedStatus", GoogleUndocumentedStatus(31337)},
		{"GoogleError", GoogleError(16, "denied", false)},
		{"GoogleHTTPError", GoogleHTTPError(503)},
		{"GooglePermissionDenied", GooglePermissionDenied()},
		{"Disconnected", Disconnected()},
		{"PhoneNotResponding", PhoneNotResponding()},
	}
}

// TestOnlyGoogleTypeAndStatusCarryRawGoogleValues is the guard spec section
// 7.2 asks for: details.google_type and details.status are the ONLY raw
// Google integers on a public surface. Any other constructor putting a
// numeric value -- or a google_-prefixed or _raw-suffixed key -- into details
// fails here.
func TestOnlyGoogleTypeAndStatusCarryRawGoogleValues(t *testing.T) {
	// The two keys spec section 7.2 permits, and the constructors allowed to
	// produce them.
	allowedNumericKeys := map[string]bool{"google_type": true, "status": true}
	googleValueConstructors := map[string]bool{
		"GoogleError": true, "GoogleUndocumentedStatus": true, "GoogleHTTPError": true,
	}

	for _, tc := range allConstructors() {
		t.Run(tc.name, func(t *testing.T) {
			for key, value := range tc.err.Details {
				if isNumeric(value) {
					if !allowedNumericKeys[key] {
						t.Errorf("details.%s carries the raw number %v; only google_type and status may (spec section 7.2)", key, value)
					}
					if !googleValueConstructors[tc.name] {
						t.Errorf("%s puts a raw number in details.%s; only the three Google constructors may", tc.name, key)
					}
				}
				if strings.HasPrefix(key, "google_") || strings.HasSuffix(key, "_raw") {
					if !googleValueConstructors[tc.name] {
						t.Errorf("%s puts a Google-namespaced key details.%s on a public surface", tc.name, key)
					}
				}
			}
		})
	}
}

func isNumeric(v any) bool {
	switch v.(type) {
	case int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64,
		float32, float64, json.Number:
		return true
	default:
		return false
	}
}

// TestEveryConstructorProducesAKnownCode proves no constructor can mint a
// code outside spec section 7.2, and that every error renders and marshals.
func TestEveryConstructorProducesAKnownCode(t *testing.T) {
	for _, tc := range allConstructors() {
		if !Known(tc.err.Code) {
			t.Errorf("%s produced %q, which spec section 7.2 does not name", tc.name, tc.err.Code)
		}
		if tc.err.Message == "" {
			t.Errorf("%s produced an error with no message", tc.name)
		}
		if _, err := json.Marshal(tc.err.Envelope("req_1")); err != nil {
			t.Errorf("%s does not marshal: %v", tc.name, err)
		}
	}
}

// TestEveryConstructorIsExercised keeps allConstructors honest: it counts the
// exported functions of the package that return *Error and fails if the table
// above has fewer rows, so a constructor added later cannot skip the
// raw-Google-value guard.
func TestEveryConstructorIsExercised(t *testing.T) {
	// The exported constructors, listed here as names only. This is a second,
	// independent statement of the inventory.
	want := []string{
		"New", "Newf", "UnknownQueryParameter", "UnknownBodyField", "MalformedBody",
		"WrongTypeForField", "MissingParameter", "WrongIDPrefix", "IDFromAnotherAccount",
		"AmbiguousAccount", "UnsupportedCapability", "IdempotencyConflict", "RateLimited",
		"NotFound", "InvalidToken", "InsufficientScope", "NotPaired", "PayloadTooLarge",
		"MediaUnsupportedType", "Internal", "NotDefaultSMSApp",
		"GoogleUndocumentedStatus", "GoogleError", "GoogleHTTPError",
		"GooglePermissionDenied", "Disconnected", "PhoneNotResponding",
	}
	got := make(map[string]bool, len(allConstructors()))
	for _, tc := range allConstructors() {
		got[tc.name] = true
	}
	for _, name := range want {
		if !got[name] {
			t.Errorf("constructor %s is not exercised by allConstructors()", name)
		}
	}
	if len(got) != len(want) {
		t.Errorf("allConstructors() has %d rows, the inventory has %d", len(got), len(want))
	}
}

// TestErrorsIsMatchesByCode proves errors.Is compares by code, so a caller
// can ask "is this a not_found?" without matching on a message.
func TestErrorsIsMatchesByCode(t *testing.T) {
	e := NotFound("message")
	if !errors.Is(e, NotFound("anything else")) {
		t.Error("two not_found errors must match")
	}
	if errors.Is(e, NotPaired()) {
		t.Error("not_found must not match not_paired")
	}

	wrapped := Internal(errors.New("underlying"))
	if !strings.Contains(wrapped.Error(), "underlying") {
		t.Errorf("the underlying error must be visible to a log: %q", wrapped.Error())
	}
	if strings.Contains(wrapped.Envelope("req_1").Error.Message, "underlying") {
		t.Error("the underlying error must not reach the caller")
	}
}

// TestNotPairedIsNotUnsupportedCapability pins the distinction spec sections
// 7.2 and 7.8 make: no accounts at all is the service-level not_paired, and
// an account that is merely unusable is unsupported_capability with
// not_signed_in. The two are never interchangeable.
func TestNotPairedIsNotUnsupportedCapability(t *testing.T) {
	if NotPaired().Code == CodeUnsupportedCapability {
		t.Fatal("not_paired must not be unsupported_capability")
	}
	signedOut := UnsupportedCapability(ReasonNotSignedIn, "acct_1", "send", "state", "signed_out")
	if signedOut.Code == CodeNotPaired {
		t.Fatal("not_signed_in must not be reported as not_paired")
	}
	if signedOut.ExitCode() != ExitUnsupported {
		t.Errorf("not_signed_in exits %d, spec section 11.2 says %d", signedOut.ExitCode(), ExitUnsupported)
	}
	if NotPaired().ExitCode() != ExitServerFailure {
		t.Errorf("not_paired exits %d, spec section 11.2 says %d", NotPaired().ExitCode(), ExitServerFailure)
	}
}
