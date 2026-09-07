package audit

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strings"
)

// RedactionError is a payload refused on the way in. It names the class of
// secret and the path it was found at, and it never names the value: an error
// string that quotes the token it is refusing to store has stored the token
// in a log instead.
type RedactionError struct {
	// Class is the forbidden class, e.g. "message text" or "google cookie".
	Class string
	// Path is the JSON path within the payload, e.g. "attempt.access_token".
	Path string
}

func (e *RedactionError) Error() string {
	return fmt.Sprintf("audit: refusing to store %s at payload path %q; "+
		"audit payloads carry IDs, counts, codes and outcomes only (spec 12.4)", e.Class, e.Path)
}

// processSalt is derived once per process and never leaves it. It is the salt
// for the phone-number hash of section 12.2: a hash that lets two rows about
// the same number be correlated, and does not let the number be recovered or
// looked up from a rainbow table. It is deliberately per-process rather than
// stored, so nothing durable ever holds the material that would make the
// hashes reversible across restarts.
//
// It is never logged. Nothing in this package prints it, returns it, or puts
// it in an error.
var processSalt = newSalt()

func newSalt() []byte {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		// Without a salt there is no safe way to record a phone number at
		// all, so this is not a condition to carry on through.
		panic("audit: cannot derive the phone-hash salt: " + err.Error())
	}
	return b
}

// Redactor scrubs and refuses payloads. Use NewRedactor; the zero value is
// not usable.
type Redactor struct {
	salt []byte
}

// NewRedactor returns a redactor using this process's salt.
func NewRedactor() *Redactor { return &Redactor{salt: processSalt} }

// newRedactorWithSalt is for the test that proves two salts give two hashes.
func newRedactorWithSalt(salt []byte) *Redactor { return &Redactor{salt: salt} }

// forbiddenKeys maps a normalised field name to the class it belongs to.
// Normalisation lowercases and drops every non-alphanumeric character, so
// `Access-Token`, `access_token` and `accessToken` are all the same key.
//
// The list errs towards refusal. A caller who genuinely wants to record a
// count under `data` can call it `byte_count`; a caller who quietly records
// an attachment under `data` because the redactor allowed it cannot be
// undone.
var forbiddenKeys = func() map[string]string {
	m := map[string]string{}
	add := func(class string, names ...string) {
		for _, n := range names {
			m[normaliseKey(n)] = class
		}
	}
	add("message text",
		"text", "message_text", "body", "message_body", "subject", "snippet",
		"preview", "caption", "content", "message_content", "rich_text", "quoted_text")
	add("a token",
		"token", "access_token", "refresh_token", "id_token", "bearer", "bearer_token",
		"authorization_header", "session_token", "tachyon_token", "refresh_key",
		"api_key", "apikey", "credential", "credentials", "password", "passphrase",
		"crypto_key", "request_crypto_key", "signing_key", "private_key")
	add("a cookie", "cookie", "cookies", "set_cookie", "cookie_header")
	// The seven Google account cookies of section 3.2, by name.
	add("a google cookie", GoogleCookieNames...)
	add("the admin secret", "admin_secret", "secret")
	add("the data key", "data_key", "datakey", "encryption_key")
	add("an enrollment code", "enrollment_code", "enrollment_code_value")
	add("an oauth authorization code",
		"authorization_code", "auth_code", "oauth_code", "code_verifier")
	add("a google account address",
		"email", "email_address", "user_email", "google_account", "google_account_address",
		"account_address", "gaia_email")
	add("attachment bytes",
		"bytes", "data", "blob", "raw", "raw_bytes", "attachment_bytes", "file_bytes",
		"payload_bytes", "media_bytes", "image", "image_data", "thumbnail", "thumbnail_bytes")
	return m
}()

// GoogleCookieNames are the seven Google account cookies of section 3.2. They
// are exported so that a log scanner and a test can enumerate the same list
// this redactor refuses.
var GoogleCookieNames = []string{
	"SID", "HSID", "OSID", "SSID", "APISID", "SAPISID", "__Secure-1PSIDTS",
}

// phoneKeys are field names whose value is a phone number whatever it looks
// like. Their values are hashed rather than refused: the whole point of the
// section 12.2 rule is that a phone number can still be *correlated* across
// rows, just never read.
//
// `from` and `to` are deliberately NOT in this list, and the reason is
// section 12.4's own wording: `account.state_changed` carries "(from, to,
// state_reason)", where both values are state names. Hashing them
// unconditionally would turn every state change into a pair of salted digests
// -- a row that records that something changed and then refuses to say what
// to. A phone number under either key is still hashed, by scrubString, which
// recognises one by its shape; what is given up is only the hashing of a
// phone number that does not look like one under those two keys.
var phoneKeys = func() map[string]bool {
	m := map[string]bool{}
	for _, n := range []string{
		"phone", "phone_number", "number", "msisdn", "e164", "participant_address",
		"address", "recipient", "sender", "destination", "caller",
	} {
		m[normaliseKey(n)] = true
	}
	return m
}()

func normaliseKey(k string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(k) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

var (
	// cookiePairRe catches a cookie header, or one cookie of it, hidden in a
	// free-form string value under an innocent key.
	cookiePairRe = regexp.MustCompile(`(?i)(^|[;,\s])(__Secure-1PSIDTS|SAPISID|APISID|HSID|OSID|SSID|SID)\s*=`)
	// bearerRe catches "Authorization: Bearer <something>" and its relatives.
	bearerRe = regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9._~+/=-]{8,}`)
	// emailRe catches a Google account address anywhere in a string. Section
	// 12.2: the acct_ ID is served instead, everywhere but /v1/accounts and
	// /v1/health.
	emailRe = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`)
	// e164Re catches a phone number embedded in a longer string. It requires
	// the leading `+`, which is what keeps it from matching the all-digit
	// groups of a UUID.
	e164Re = regexp.MustCompile(`\+\d[\d\s().-]{5,17}\d`)
	// bareNumberRe decides whether a whole string is a phone number: digits
	// and separators, nothing else, seven to fifteen digits.
	bareNumberRe = regexp.MustCompile(`^\+?[\d\s().-]+$`)
)

// Payload redacts one payload, returning the value that will be marshalled
// into `payload_json`.
//
// It is applied by the Writer to every event. There is no path that stores a
// payload without it, which is the difference between redaction as a property
// and redaction as a rule callers are asked to remember.
func (r *Redactor) Payload(p map[string]any) (map[string]any, error) {
	if p == nil {
		return map[string]any{}, nil
	}
	v, err := r.value("", p)
	if err != nil {
		return nil, err
	}
	out, _ := v.(map[string]any)
	if out == nil {
		out = map[string]any{}
	}
	return out, nil
}

func join(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

// value walks one payload value.
func (r *Redactor) value(path string, v any) (any, error) {
	if v == nil {
		return nil, nil
	}
	rv := reflect.ValueOf(v)
	for rv.Kind() == reflect.Pointer || rv.Kind() == reflect.Interface {
		if rv.IsNil() {
			return nil, nil
		}
		rv = rv.Elem()
	}

	switch rv.Kind() {
	case reflect.String:
		return r.scrubString(path, rv.String())

	case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32,
		reflect.Int64, reflect.Uint, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return rv.Interface(), nil

	case reflect.Uint8:
		return rv.Interface(), nil

	case reflect.Slice, reflect.Array:
		// A []byte is attachment bytes, a key, a token or a ciphertext. It is
		// never a count, and it is never something an audit row needs.
		if rv.Type().Elem().Kind() == reflect.Uint8 {
			return nil, &RedactionError{Class: "raw bytes", Path: pathOrRoot(path)}
		}
		out := make([]any, 0, rv.Len())
		for i := 0; i < rv.Len(); i++ {
			e, err := r.value(fmt.Sprintf("%s[%d]", pathOrRoot(path), i), rv.Index(i).Interface())
			if err != nil {
				return nil, err
			}
			out = append(out, e)
		}
		return out, nil

	case reflect.Map:
		if rv.Type().Key().Kind() != reflect.String {
			return nil, fmt.Errorf("audit: payload map at %q must be keyed by string, not %s",
				pathOrRoot(path), rv.Type().Key())
		}
		out := make(map[string]any, rv.Len())
		keys := make([]string, 0, rv.Len())
		for _, k := range rv.MapKeys() {
			keys = append(keys, k.String())
		}
		sort.Strings(keys)
		for _, k := range keys {
			child := join(path, k)
			norm := normaliseKey(k)
			if class, bad := forbiddenKeys[norm]; bad {
				return nil, &RedactionError{Class: class, Path: child}
			}
			ev := rv.MapIndex(reflect.ValueOf(k).Convert(rv.Type().Key())).Interface()
			if phoneKeys[norm] {
				s, isStr := asString(ev)
				if !isStr {
					return nil, &RedactionError{Class: "a phone number", Path: child}
				}
				out[k] = r.PhoneRef(s)
				continue
			}
			red, err := r.value(child, ev)
			if err != nil {
				return nil, err
			}
			out[k] = red
		}
		return out, nil
	}

	// Structs and everything else are refused rather than reflected over:
	// an audit payload is IDs, counts, codes and outcomes, and a struct is
	// how a whole message or a whole session object gets in by accident.
	return nil, fmt.Errorf("audit: payload value at %q is a %s; "+
		"audit payloads carry only strings, numbers, booleans, maps and slices (spec 12.4)",
		pathOrRoot(path), rv.Kind())
}

func pathOrRoot(p string) string {
	if p == "" {
		return "(root)"
	}
	return p
}

func asString(v any) (string, bool) {
	rv := reflect.ValueOf(v)
	for rv.Kind() == reflect.Pointer || rv.Kind() == reflect.Interface {
		if rv.IsNil() {
			return "", false
		}
		rv = rv.Elem()
	}
	if rv.Kind() == reflect.String {
		return rv.String(), true
	}
	return "", false
}

// scrubString refuses the classes that cannot be recorded in any form, and
// rewrites the one class that can: a phone number becomes a salted hash plus
// its last four digits.
func (r *Redactor) scrubString(path, s string) (any, error) {
	switch {
	case cookiePairRe.MatchString(s):
		return nil, &RedactionError{Class: "a google cookie", Path: pathOrRoot(path)}
	case bearerRe.MatchString(s):
		return nil, &RedactionError{Class: "a token", Path: pathOrRoot(path)}
	case emailRe.MatchString(s):
		return nil, &RedactionError{Class: "a google account address", Path: pathOrRoot(path)}
	}
	if digits := onlyDigits(s); bareNumberRe.MatchString(s) && len(digits) >= 7 && len(digits) <= 15 {
		return r.PhoneRef(s), nil
	}
	// A number embedded in a longer string, which must carry a `+` to be
	// distinguishable from an identifier.
	if e164Re.MatchString(s) {
		return e164Re.ReplaceAllStringFunc(s, func(m string) string {
			h, last := r.phoneParts(m)
			return "[phone:" + h + "/" + last + "]"
		}), nil
	}
	return s, nil
}

func onlyDigits(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// PhoneRef is what an audit payload carries in place of a phone number: a
// stable salted hash so two rows about the same number can be correlated, and
// the last four digits so an owner can recognise it (section 12.2).
func (r *Redactor) PhoneRef(number string) map[string]any {
	h, last := r.phoneParts(number)
	return map[string]any{"phone_hash": h, "last4": last}
}

func (r *Redactor) phoneParts(number string) (hash, last4 string) {
	digits := onlyDigits(number)
	sum := sha256.New()
	sum.Write(r.salt)
	sum.Write([]byte("phone:"))
	sum.Write([]byte(digits))
	hash = hex.EncodeToString(sum.Sum(nil))[:16]
	last4 = digits
	if len(digits) > 4 {
		last4 = digits[len(digits)-4:]
	}
	return hash, last4
}
