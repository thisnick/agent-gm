package audit

import (
	"context"
	"strings"
	"testing"

	"github.com/thisnick/agent-gm/internal/clock"
)

// Fictional numbers only. +1 202 555 xxxx is the reserved-for-fiction range;
// no real number appears anywhere in this repository.
const (
	fictionalNumberA = "+12025550143"
	fictionalNumberB = "+12025550188"
)

// forbidden is one class of section 12.2 secret, a sentinel value for it, and
// the payload that carries it. The sentinel is a distinctive string so the
// assertion can be the blunt one: it must not survive anywhere into
// payload_json, and Append must have refused.
type forbidden struct {
	class    string
	sentinel string
	payload  map[string]any
}

func forbiddenCases() []forbidden {
	return []forbidden{
		{"message text", "SENTINEL-MESSAGE-BODY",
			map[string]any{"text": "SENTINEL-MESSAGE-BODY"}},
		{"message text nested", "SENTINEL-NESTED-BODY",
			map[string]any{"message": map[string]any{"body": "SENTINEL-NESTED-BODY"}}},
		{"subject", "SENTINEL-SUBJECT",
			map[string]any{"subject": "SENTINEL-SUBJECT"}},
		{"message text in a list", "SENTINEL-LIST-SNIPPET",
			map[string]any{"messages": []any{map[string]any{"snippet": "SENTINEL-LIST-SNIPPET"}}}},
		{"access token", "SENTINEL-ACCESS-TOKEN",
			map[string]any{"access_token": "SENTINEL-ACCESS-TOKEN"}},
		{"refresh token", "SENTINEL-REFRESH-TOKEN",
			map[string]any{"refreshToken": "SENTINEL-REFRESH-TOKEN"}},
		{"tachyon token", "SENTINEL-TACHYON",
			map[string]any{"tachyon_token": "SENTINEL-TACHYON"}},
		{"bearer token in a free string", "SENTINEL-BEARER-VALUE",
			map[string]any{"note": "Authorization: Bearer SENTINEL-BEARER-VALUE"}},
		{"admin secret", "SENTINEL-ADMIN-SECRET",
			map[string]any{"admin_secret": "SENTINEL-ADMIN-SECRET"}},
		{"data key", "SENTINEL-DATA-KEY",
			map[string]any{"data_key": "SENTINEL-DATA-KEY"}},
		{"enrollment code", "SENTINEL-ENROLLMENT-CODE",
			map[string]any{"enrollment_code": "SENTINEL-ENROLLMENT-CODE"}},
		{"oauth authorization code", "SENTINEL-OAUTH-CODE",
			map[string]any{"authorization_code": "SENTINEL-OAUTH-CODE"}},
		{"pkce verifier", "SENTINEL-VERIFIER",
			map[string]any{"code_verifier": "SENTINEL-VERIFIER"}},
		{"google account address", "sentinel.owner@example.com",
			map[string]any{"who": "sentinel.owner@example.com"}},
		{"google account address under an email key", "sentinel-two@example.com",
			map[string]any{"email": "sentinel-two@example.com"}},
		{"attachment bytes as raw bytes", "SENTINEL-ATTACHMENT",
			map[string]any{"attachment": []byte("SENTINEL-ATTACHMENT")}},
		{"attachment bytes under a data key", "SENTINEL-DATA-FIELD",
			map[string]any{"data": "SENTINEL-DATA-FIELD"}},
		{"cookie header in a free string", "SENTINEL-COOKIE-JAR",
			map[string]any{"detail": "SID=SENTINEL-COOKIE-JAR; HSID=x"}},
	}
}

// TestRedactionRefusesEverySection122Class stuffs a payload with a sentinel
// for each forbidden class and asserts none of them survives.
//
// This is the test the package exists for. Redaction is applied by the writer
// to every payload on the way in, so a caller cannot forget it; what this
// proves is that the redactor actually catches each class rather than the
// two obvious ones.
func TestRedactionRefusesEverySection122Class(t *testing.T) {
	ctx := context.Background()
	for _, c := range forbiddenCases() {
		t.Run(c.class, func(t *testing.T) {
			w, a := newTestWriter(t)
			e := ok(KindSettingsChanged)
			e.Payload = c.payload
			err := w.Append(ctx, e)
			if err == nil {
				t.Fatalf("a payload carrying %s was accepted", c.class)
			}
			if strings.Contains(err.Error(), c.sentinel) {
				t.Errorf("the refusal itself quotes the secret: %q", err)
			}
			if n := len(a.Rows()); n != 0 {
				t.Fatalf("a refused payload wrote %d rows: %+v", n, a.Rows())
			}
		})
	}
}

// TestEveryGoogleCookieNameIsRefusedByName covers the seven of section 3.2
// individually, both as a field name and as a cookie pair inside a string,
// so a rename upstream cannot quietly drop one.
func TestEveryGoogleCookieNameIsRefusedByName(t *testing.T) {
	ctx := context.Background()
	if len(GoogleCookieNames) != 7 {
		t.Fatalf("section 3.2 names seven cookies, this package knows %d", len(GoogleCookieNames))
	}
	for _, name := range GoogleCookieNames {
		sentinel := "SENTINEL-COOKIE-" + name
		payloads := []map[string]any{
			{name: sentinel},
			{"captured": name + "=" + sentinel},
			{"headers": map[string]any{"cookie": sentinel}},
		}
		for i, p := range payloads {
			w, a := newTestWriter(t)
			e := ok(KindSettingsChanged)
			e.Payload = p
			if err := w.Append(ctx, e); err == nil {
				t.Errorf("%s payload %d: cookie value was accepted", name, i)
			}
			for _, row := range a.Rows() {
				if strings.Contains(row.PayloadJSON, sentinel) {
					t.Errorf("%s payload %d: value survived into %s", name, i, row.PayloadJSON)
				}
			}
		}
	}
}

// TestPhoneNumbersBecomeHashPlusLastFour is section 12.2's one scrub-rather-
// than-refuse rule: an audit row must still let two events about the same
// number be correlated, and must never carry the number.
func TestPhoneNumbersBecomeHashPlusLastFour(t *testing.T) {
	ctx := context.Background()
	w, a := newTestWriter(t)

	e := ok(KindOperationSend)
	e.AccountID = "acct_1"
	e.Payload = map[string]any{
		"recipient":  fictionalNumberA,
		"other":      fictionalNumberB,
		"bare":       "2025550143",
		"prose":      "delivery to " + fictionalNumberA + " failed",
		"conv_id":    "conv_5ec9ab28-5363-57f8-b543-27672479604a",
		"attempt":    2,
		"error_code": "phone_not_responding",
	}
	if err := w.Append(ctx, e); err != nil {
		t.Fatalf("a payload of IDs, counts and codes was refused: %v", err)
	}
	stored := a.Rows()[0].PayloadJSON

	for _, digits := range []string{"12025550143", "2025550143", "12025550188", "2025550188"} {
		if strings.Contains(stored, digits) {
			t.Fatalf("a full phone number survived into the payload: %s", stored)
		}
	}
	if !strings.Contains(stored, `"last4":"0143"`) || !strings.Contains(stored, `"last4":"0188"`) {
		t.Errorf("the last four digits are missing from %s", stored)
	}
	if !strings.Contains(stored, `"phone_hash"`) {
		t.Errorf("the salted hash is missing from %s", stored)
	}
	// The conversation ID, which is not a phone number, is untouched.
	if !strings.Contains(stored, "conv_5ec9ab28-5363-57f8-b543-27672479604a") {
		t.Errorf("an ID was mistaken for a phone number: %s", stored)
	}
	if !strings.Contains(stored, "phone_not_responding") || !strings.Contains(stored, `"attempt":2`) {
		t.Errorf("codes and counts must survive: %s", stored)
	}
	// The prose case keeps its shape and loses its number.
	if !strings.Contains(stored, "delivery to [phone:") {
		t.Errorf("an embedded number was not rewritten in place: %s", stored)
	}
}

// TestPhoneHashIsStableAndCollisionFree: the same number twice gives the same
// hash within a process, two different numbers give different ones, and the
// same number written two ways is one number.
func TestPhoneHashIsStableAndCollisionFree(t *testing.T) {
	r := NewRedactor()

	a1 := r.PhoneRef(fictionalNumberA)
	a2 := r.PhoneRef(fictionalNumberA)
	if a1["phone_hash"] != a2["phone_hash"] {
		t.Fatalf("the same number hashed to %v and %v", a1, a2)
	}
	spaced := r.PhoneRef("+1 (202) 555-0143")
	if spaced["phone_hash"] != a1["phone_hash"] {
		t.Fatalf("the same number written two ways hashed to %v and %v", spaced, a1)
	}
	b := r.PhoneRef(fictionalNumberB)
	if b["phone_hash"] == a1["phone_hash"] {
		t.Fatalf("two different numbers collided on %v", b["phone_hash"])
	}
	if a1["last4"] != "0143" || b["last4"] != "0188" {
		t.Fatalf("last4 = %v and %v", a1["last4"], b["last4"])
	}
	if h, _ := a1["phone_hash"].(string); len(h) != 16 {
		t.Fatalf("hash %q is not 16 hex characters", h)
	}

	// A different salt gives a different hash, which is what makes the
	// per-process salt worth having: the hashes are not a lookup table.
	other := newRedactorWithSalt([]byte("a different salt entirely"))
	if other.PhoneRef(fictionalNumberA)["phone_hash"] == a1["phone_hash"] {
		t.Fatal("the salt does not affect the hash")
	}
}

// TestKitchenSinkPayloadIsRefused is the whole list at once, which is how a
// real leak would arrive: someone dumps a struct into a payload.
func TestKitchenSinkPayloadIsRefused(t *testing.T) {
	w, a := newTestWriter(t)
	e := ok(KindSettingsChanged)
	e.Payload = map[string]any{
		"text":               "SENTINEL-BODY",
		"access_token":       "SENTINEL-TOKEN",
		"SID":                "SENTINEL-SID",
		"admin_secret":       "SENTINEL-SECRET",
		"data_key":           "SENTINEL-KEY",
		"enrollment_code":    "SENTINEL-ENROLL",
		"authorization_code": "SENTINEL-CODE",
		"email":              "sentinel@example.com",
		"bytes":              []byte("SENTINEL-BYTES"),
		"phone":              fictionalNumberA,
	}
	if err := w.Append(context.Background(), e); err == nil {
		t.Fatal("the kitchen sink was accepted")
	}
	if len(a.Rows()) != 0 {
		t.Fatalf("stored %+v", a.Rows())
	}
}

// TestStructPayloadsAreRefused: a struct in a payload is how a whole message
// or a whole session object gets in by accident.
func TestStructPayloadsAreRefused(t *testing.T) {
	type message struct{ Text string }
	w, _ := newTestWriter(t)
	e := ok(KindSettingsChanged)
	e.Payload = map[string]any{"m": message{Text: "SENTINEL"}}
	err := w.Append(context.Background(), e)
	if err == nil || !strings.Contains(err.Error(), "struct") {
		t.Fatalf("a struct payload was accepted or misreported: %v", err)
	}
}

func TestNilPayloadIsAnEmptyObject(t *testing.T) {
	w, a := newTestWriter(t)
	e := ok(KindSettingsChanged)
	e.Payload = nil
	if err := w.Append(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if got := a.Rows()[0].PayloadJSON; got != "{}" {
		t.Fatalf("payload_json = %q, want {}", got)
	}
}

// TestRedactionIsNotOptional: there is no exported way to build a Writer that
// skips redaction, and the redactor is not a field a caller can nil out.
func TestRedactionIsNotOptional(t *testing.T) {
	w := NewWriter(NewMemoryAppender(), clock.NewFake())
	if w.redactor == nil {
		t.Fatal("NewWriter built a writer with no redactor")
	}
	e := ok(KindSettingsChanged)
	e.Payload = map[string]any{"text": "SENTINEL"}
	if err := w.Append(context.Background(), e); err == nil {
		t.Fatal("redaction was skipped")
	}
}
