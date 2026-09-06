//go:build fixtures

// Package upstream holds the fixture-validation job of spec section 13.4.
//
// Every assertion here reads the pinned mautrix-gmessages tree, not Agent
// GM's own code. It runs under `devbox run fixture-validation`, which clones
// the tree at the pinned commit first and points AGENT_GM_UPSTREAM_DIR at it.
package upstream

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/thisnick/agent-gm/internal/gm"
)

func upstreamDir(t *testing.T) string {
	t.Helper()
	dir := os.Getenv("AGENT_GM_UPSTREAM_DIR")
	if dir == "" {
		t.Fatal("AGENT_GM_UPSTREAM_DIR is not set; run `devbox run fixture-validation`")
	}
	if _, err := os.Stat(filepath.Join(dir, "pkg", "libgm", "client.go")); err != nil {
		t.Fatalf("AGENT_GM_UPSTREAM_DIR=%s does not look like a mautrix-gmessages checkout: %v", dir, err)
	}
	return dir
}

func read(t *testing.T, dir string, rel ...string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(append([]string{dir}, rel...)...))
	if err != nil {
		t.Fatalf("reading %v: %v", rel, err)
	}
	return string(data)
}

func mustContain(t *testing.T, haystack, needle, what string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Errorf("%s: expected to find %q in the pinned tree", what, needle)
	}
}

// grepTree returns every file:line in the tree matching re, excluding paths
// containing any of skip.
func grepTree(t *testing.T, dir string, re *regexp.Regexp, skip ...string) []string {
	t.Helper()
	var hits []string
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if info.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		rel, _ := filepath.Rel(dir, path)
		for _, s := range skip {
			if strings.Contains(rel, s) {
				return nil
			}
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for i, line := range strings.Split(string(data), "\n") {
			if re.MatchString(line) {
				hits = append(hits, fmt.Sprintf("%s:%d", rel, i+1))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", dir, err)
	}
	return hits
}

// enumValues parses `NAME = N;` entries out of a proto enum block.
func enumValues(t *testing.T, proto, enumName string) map[string]int32 {
	t.Helper()
	// Match `enum NAME {` and read to the matching close brace.
	idx := strings.Index(proto, "enum "+enumName+" {")
	if idx < 0 {
		t.Fatalf("enum %s not found in proto", enumName)
	}
	rest := proto[idx:]
	end := strings.Index(rest, "\n}")
	if end < 0 {
		t.Fatalf("enum %s has no closing brace", enumName)
	}
	body := rest[:end]
	out := map[string]int32{}
	re := regexp.MustCompile(`(?m)^\s*([A-Z][A-Z0-9_]*)\s*=\s*(\d+)\s*;`)
	for _, m := range re.FindAllStringSubmatch(body, -1) {
		n, err := strconv.Atoi(m[2])
		if err != nil {
			t.Fatalf("enum %s value %s: %v", enumName, m[2], err)
		}
		out[m[1]] = int32(n)
	}
	if len(out) == 0 {
		t.Fatalf("enum %s parsed to zero values", enumName)
	}
	return out
}

// Assertion 1: every symbol named in spec section 3.1 exists with the
// signature stated.
func TestAssertion01_SectionThreeOneSignatures(t *testing.T) {
	dir := upstreamDir(t)
	client := read(t, dir, "pkg", "libgm", "client.go")
	methods := read(t, dir, "pkg", "libgm", "methods.go")
	media := read(t, dir, "pkg", "libgm", "media.go")
	pairGoogle := read(t, dir, "pkg", "libgm", "pair_google.go")

	for _, c := range []struct{ file, sig string }{
		{"client.go", "func NewAuthData() *AuthData {"},
		{"client.go", "func NewClient(authData *AuthData, pk *PushKeys, logger zerolog.Logger) *Client {"},
		{"client.go", "func (c *Client) SetEventHandler(eventHandler EventHandler) {"},
		{"client.go", "func (c *Client) FetchConfig(ctx context.Context) error {"},
		{"client.go", "func (c *Client) Connect() error {"},
		{"client.go", "func (c *Client) ConnectBackground() error {"},
		{"client.go", "func (c *Client) Disconnect() {"},
		{"client.go", "func (c *Client) Reconnect() error {"},
		{"client.go", "func (c *Client) IsConnected() bool {"},
		{"client.go", "func (c *Client) IsLoggedIn() bool {"},
		{"client.go", "func (c *Client) CurrentSessionID() string {"},
		{"client.go", "func (c *Client) SetProxy(proxy string) error {"},
		{"client.go", "func (c *Client) SetPingInterval(interval time.Duration) {"},
		{"client.go", "func (c *Client) SetDataReceiveCheckInterval(interval time.Duration) {"},
		{"client.go", "type EventHandler func(evt any)"},
		{"client.go", "GaiaHackyDeviceSwitcher int"},
		{"client.go", "func (ad *AuthData) IsGoogleAccount() bool {"},
		{"client.go", "func (ad *AuthData) AuthNetwork() string {"},
		{"client.go", "func (ad *AuthData) HasCookies() bool {"},
		{"client.go", "func (ad *AuthData) SetCookies(cookies map[string]string) {"},
		{"client.go", "func (ad *AuthData) UpdateCookiesFromResponse(resp *http.Response) {"},
	} {
		mustContain(t, client, c.sig, "section 3.1 "+c.file)
	}

	for _, sig := range []string{
		"func (c *Client) ListConversations(ctx context.Context, count int, folder gmproto.ListConversationsRequest_Folder) (*gmproto.ListConversationsResponse, error) {",
		"func (c *Client) GetConversation(ctx context.Context, conversationID string) (*gmproto.Conversation, error) {",
		"func (c *Client) GetConversationType(ctx context.Context, conversationID string) (*gmproto.GetConversationTypeResponse, error) {",
		"func (c *Client) FetchMessages(ctx context.Context, conversationID string, count int64, cursor *gmproto.Cursor) (*gmproto.ListMessagesResponse, error) {",
		"func (c *Client) ListContacts(ctx context.Context) (*gmproto.ListContactsResponse, error) {",
		"func (c *Client) ListTopContacts(ctx context.Context) (*gmproto.ListTopContactsResponse, error) {",
		"func (c *Client) IsBugleDefault(ctx context.Context) (*gmproto.IsBugleDefaultResponse, error) {",
		"func (c *Client) GetParticipantThumbnail(ctx context.Context, participantIDs ...string) (*gmproto.GetThumbnailResponse, error) {",
		"func (c *Client) GetContactThumbnail(ctx context.Context, contactIDs ...string) (*gmproto.GetThumbnailResponse, error) {",
		"func (c *Client) SendMessage(ctx context.Context, payload *gmproto.SendMessageRequest) (*gmproto.SendMessageResponse, error) {",
		"func (c *Client) SendReaction(ctx context.Context, payload *gmproto.SendReactionRequest) (*gmproto.SendReactionResponse, error) {",
		"func (c *Client) DeleteMessage(ctx context.Context, messageID string) (*gmproto.DeleteMessageResponse, error) {",
		"func (c *Client) MarkRead(ctx context.Context, conversationID, messageID string) error {",
		"func (c *Client) SetTyping(ctx context.Context, convID string, simPayload *gmproto.SIMPayload) error {",
		"func (c *Client) GetOrCreateConversation(ctx context.Context, req *gmproto.GetOrCreateConversationRequest) (*gmproto.GetOrCreateConversationResponse, error) {",
		"func (c *Client) UpdateConversation(ctx context.Context, payload *gmproto.UpdateConversationRequest) (*gmproto.UpdateConversationResponse, error) {",
		"func (c *Client) DeleteConversation(ctx context.Context, conversationID, phone string) error {",
		"func (c *Client) GetFullSizeImage(ctx context.Context, messageID, actionMessageID string) (*gmproto.GetFullSizeImageResponse, error) {",
	} {
		mustContain(t, methods, sig, "section 3.1 methods.go")
	}

	for _, sig := range []string{
		// Note these two take no context.Context at the pinned commit; the
		// adapter wraps each in a goroutine plus a select on ctx.Done().
		"func (c *Client) UploadMedia(data []byte, fileName, mime string) (*gmproto.MediaContent, error) {",
		"func (c *Client) DownloadMedia(mediaID string, key []byte) ([]byte, error) {",
		"func (c *Client) DownloadAvatar(ctx context.Context, url string) ([]byte, error) {",
		"var MimeToMediaType = map[string]MediaType{",
	} {
		mustContain(t, media, sig, "section 3.1 media.go")
	}

	for _, sig := range []string{
		"func (c *Client) DoGaiaPairing(ctx context.Context, emojiCallback func(string)) error {",
		"func (c *Client) StartGaiaPairing(ctx context.Context) (string, *PairingSession, error) {",
		"func (c *Client) FinishGaiaPairing(ctx context.Context, ps *PairingSession) (string, error) {",
	} {
		mustContain(t, pairGoogle, sig, "section 3.1 pair_google.go")
	}

	// PairCallback exists and is only ever read from completePairing, which
	// the gaia path never reaches. Leaving it nil is part of the contract.
	mustContain(t, client, "PairCallback atomic.Pointer[func(data *gmproto.PairedData)]", "section 3.1 PairCallback")
}

// Assertion 2: util.ConfigMessage equals 2026.9.2 with V1=4, V2=6.
func TestAssertion02_ConfigVersion(t *testing.T) {
	dir := upstreamDir(t)
	cfg := read(t, dir, "pkg", "libgm", "util", "config.go")
	for _, want := range []string{"Year:  2026", "Month: 9", "Day:   2", "V1:    4", "V2:    6"} {
		mustContain(t, cfg, want, "util.ConfigMessage")
	}
	compiled := gm.CompiledConfigVersion()
	if compiled != (gm.ConfigVersion{Year: 2026, Month: 9, Day: 2, V1: 4, V2: 6}) {
		t.Errorf("the compiled ConfigVersion Agent GM reports is %s, want 2026.9.2.4.6", compiled)
	}
}

// Assertions 3 and 12: section 4.4 covers every MessageStatusType value
// outside 200-279 exactly once, with no value unmapped; every value inside
// 200-279 classifies as kind='system'; MESSAGE_DELETED is still 300.
func TestAssertion03and12_MessageStatusTypeCoverage(t *testing.T) {
	dir := upstreamDir(t)
	proto := read(t, dir, "pkg", "libgm", "gmproto", "conversations.proto")
	values := enumValues(t, proto, "MessageStatusType")

	if got, ok := values["MESSAGE_DELETED"]; !ok || got != 300 {
		t.Errorf("MESSAGE_DELETED is %d, want 300", got)
	}
	if got := gm.MessageDeletedStatus; got != 300 {
		t.Errorf("Agent GM's MessageDeletedStatus is %d, want 300", got)
	}

	mapped := map[int32]bool{}
	for _, v := range gm.MappedStatusValues() {
		mapped[v] = true
	}

	var lowSystem, highSystem int32 = 1 << 30, -1
	for name, v := range values {
		if v >= 200 && v <= 279 {
			if v < lowSystem {
				lowSystem = v
			}
			if v > highSystem {
				highSystem = v
			}
			if gm.KindForStatus(v) != gm.MessageKindSystem {
				t.Errorf("%s(%d) is in the system band but Agent GM classifies it as %s",
					name, v, gm.KindForStatus(v))
			}
			if mapped[v] {
				t.Errorf("%s(%d) is in the 200-279 band and must not appear in the delivery-state map", name, v)
			}
			continue
		}
		if !mapped[v] {
			t.Errorf("%s(%d) is declared at the pin but spec section 4.4 does not map it", name, v)
		}
		if _, ok := gm.DeliveryStateFor(v); !ok {
			t.Errorf("%s(%d) has no delivery state", name, v)
		}
	}

	// The band is exactly 200-279: nothing upstream sits outside it and is
	// still a system event.
	if lowSystem != 200 || highSystem != 279 {
		t.Errorf("the system-event band at the pin is %d-%d, spec section 4.4 says 200-279", lowSystem, highSystem)
	}

	// And nothing Agent GM maps is absent from the pinned enum.
	byValue := map[int32]string{}
	for name, v := range values {
		byValue[v] = name
	}
	for _, v := range gm.MappedStatusValues() {
		if _, ok := byValue[v]; !ok {
			t.Errorf("Agent GM maps status %d, which the pinned enum does not declare", v)
		}
	}
}

// Assertion 4: GetOrCreateConversationResponse.Status still declares only
// 0, 1, 3, so section 3.7's claim that 2 and 4 are unnamed is still true.
func TestAssertion04_GetOrCreateConversationStatus(t *testing.T) {
	dir := upstreamDir(t)
	proto := read(t, dir, "pkg", "libgm", "gmproto", "client.proto")
	idx := strings.Index(proto, "message GetOrCreateConversationResponse {")
	if idx < 0 {
		t.Fatal("GetOrCreateConversationResponse not found")
	}
	values := enumValues(t, proto[idx:], "Status")
	want := map[string]int32{"UNKNOWN": 0, "SUCCESS": 1, "CREATE_RCS": 3}
	assertEnum(t, "GetOrCreateConversationResponse.Status", values, want)
}

// Assertion 5: SendMessageResponse.Status still declares 0..4.
func TestAssertion05_SendMessageStatus(t *testing.T) {
	dir := upstreamDir(t)
	proto := read(t, dir, "pkg", "libgm", "gmproto", "client.proto")
	idx := strings.Index(proto, "message SendMessageResponse {")
	if idx < 0 {
		t.Fatal("SendMessageResponse not found")
	}
	values := enumValues(t, proto[idx:], "Status")
	assertEnum(t, "SendMessageResponse.Status", values, map[string]int32{
		"UNKNOWN": 0, "SUCCESS": 1, "FAILURE_2": 2, "FAILURE_3": 3, "FAILURE_4": 4,
	})
}

// Assertion 6: SendReactionRequest.Action still declares 0..3.
func TestAssertion06_SendReactionAction(t *testing.T) {
	dir := upstreamDir(t)
	proto := read(t, dir, "pkg", "libgm", "gmproto", "client.proto")
	idx := strings.Index(proto, "message SendReactionRequest {")
	if idx < 0 {
		t.Fatal("SendReactionRequest not found")
	}
	values := enumValues(t, proto[idx:], "Action")
	assertEnum(t, "SendReactionRequest.Action", values, map[string]int32{
		"UNSPECIFIED": 0, "ADD": 1, "REMOVE": 2, "SWITCH": 3,
	})
	for name, want := range map[string]gm.ReactionAction{
		"UNSPECIFIED": gm.ReactionActionUnspecified,
		"ADD":         gm.ReactionActionAdd,
		"REMOVE":      gm.ReactionActionRemove,
		"SWITCH":      gm.ReactionActionSwitch,
	} {
		if int32(want) != values[name] {
			t.Errorf("Agent GM's ReactionAction %s is %d, the pin says %d", name, want, values[name])
		}
	}
}

// Assertion 7: ListConversationsRequest.Folder still declares 0, 1, 2, 5.
func TestAssertion07_ListConversationsFolder(t *testing.T) {
	dir := upstreamDir(t)
	proto := read(t, dir, "pkg", "libgm", "gmproto", "client.proto")
	idx := strings.Index(proto, "message ListConversationsRequest {")
	if idx < 0 {
		t.Fatal("ListConversationsRequest not found")
	}
	values := enumValues(t, proto[idx:], "Folder")
	assertEnum(t, "ListConversationsRequest.Folder", values, map[string]int32{
		"UNKNOWN": 0, "INBOX": 1, "ARCHIVE": 2, "SPAM_BLOCKED": 5,
	})
	for name, want := range map[string]gm.Folder{
		"UNKNOWN": gm.FolderUnknown, "INBOX": gm.FolderInbox,
		"ARCHIVE": gm.FolderArchive, "SPAM_BLOCKED": gm.FolderSpamBlocked,
	} {
		if int32(want) != values[name] {
			t.Errorf("Agent GM's Folder %s is %d, the pin says %d", name, want, values[name])
		}
	}
}

// Assertion 8: AlertType still has 28 values (0-27), and the ones section 3.4
// acts on still carry the numbers stated.
func TestAssertion08_AlertType(t *testing.T) {
	dir := upstreamDir(t)
	proto := read(t, dir, "pkg", "libgm", "gmproto", "events.proto")
	values := enumValues(t, proto, "AlertType")
	if len(values) != gm.AlertTypeCount {
		t.Errorf("AlertType declares %d values, spec section 3.4 says %d", len(values), gm.AlertTypeCount)
	}
	nums := make([]int, 0, len(values))
	for _, v := range values {
		nums = append(nums, int(v))
	}
	sort.Ints(nums)
	for i, n := range nums {
		if n != i {
			t.Errorf("AlertType is not a contiguous 0..%d run: index %d is %d", len(nums)-1, i, n)
			break
		}
	}
	for name, want := range map[string]gm.AlertType{
		"BROWSER_INACTIVE":                 gm.AlertBrowserInactive,
		"BROWSER_ACTIVE":                   gm.AlertBrowserActive,
		"MOBILE_DATA_CONNECTION":           gm.AlertMobileDataConnection,
		"MOBILE_WIFI_CONNECTION":           gm.AlertMobileWifiConnection,
		"MOBILE_BATTERY_LOW":               gm.AlertMobileBatteryLow,
		"MOBILE_BATTERY_RESTORED":          gm.AlertMobileBatteryRestored,
		"BROWSER_INACTIVE_FROM_TIMEOUT":    gm.AlertBrowserInactiveFromTimeout,
		"BROWSER_INACTIVE_FROM_INACTIVITY": gm.AlertBrowserInactiveFromInactivity,
		"RCS_CONNECTION":                   gm.AlertRCSConnection,
		"MOBILE_DATABASE_SYNCING":          gm.AlertMobileDatabaseSyncing,
		"MOBILE_DATABASE_SYNC_COMPLETE":    gm.AlertMobileDatabaseSyncComplete,
		"MOBILE_DATABASE_SYNC_STARTED":     gm.AlertMobileDatabaseSyncStarted,
	} {
		got, ok := values[name]
		if !ok {
			t.Errorf("AlertType %s is gone from the pin", name)
			continue
		}
		if got != int32(want) {
			t.Errorf("AlertType %s is %d at the pin, Agent GM says %d", name, got, want)
		}
	}
}

// Assertion 9: responseHardTimeout is still 60s, RefreshTachyonBuffer still
// 1h, alertTimeoutCount still defaults to 4, GaiaInitTimeout still 20s.
func TestAssertion09_Constants(t *testing.T) {
	dir := upstreamDir(t)
	mustContain(t, read(t, dir, "pkg", "libgm", "session_handler.go"),
		"const responseHardTimeout = 60 * time.Second", "responseHardTimeout")
	mustContain(t, read(t, dir, "pkg", "libgm", "client.go"),
		"const RefreshTachyonBuffer = 1 * time.Hour", "RefreshTachyonBuffer")
	mustContain(t, read(t, dir, "pkg", "libgm", "client.go"),
		"alertTimeoutCount:        4,", "alertTimeoutCount default")
	mustContain(t, read(t, dir, "pkg", "libgm", "pair_google.go"),
		"const GaiaInitTimeout = 20 * time.Second", "GaiaInitTimeout")
}

// Assertion 10: shouldIgnoreStatus's ignore set matches the one Agent GM
// carries.
func TestAssertion10_ShouldIgnoreStatus(t *testing.T) {
	dir := upstreamDir(t)
	src := read(t, dir, "pkg", "connector", "handlegmessages.go")
	idx := strings.Index(src, "func shouldIgnoreStatus(")
	if idx < 0 {
		t.Fatal("shouldIgnoreStatus not found")
	}
	body := src[idx:]
	if end := strings.Index(body, "\n}\n"); end > 0 {
		body = body[:end]
	}
	// Split the two arms: `return isDM` ends the DM-only set.
	dmEnd := strings.Index(body, "return isDM")
	if dmEnd < 0 {
		t.Fatal("shouldIgnoreStatus has no `return isDM` arm")
	}
	nameRe := regexp.MustCompile(`gmproto\.MessageStatusType_([A-Z0-9_]+)`)
	collect := func(s string) []string {
		var out []string
		for _, m := range nameRe.FindAllStringSubmatch(s, -1) {
			out = append(out, m[1])
		}
		sort.Strings(out)
		return out
	}
	dmNames := collect(body[:dmEnd])
	alwaysNames := collect(body[dmEnd:])

	proto := read(t, dir, "pkg", "libgm", "gmproto", "conversations.proto")
	values := enumValues(t, proto, "MessageStatusType")
	toNums := func(names []string) []int32 {
		var out []int32
		for _, n := range names {
			v, ok := values[n]
			if !ok {
				t.Errorf("shouldIgnoreStatus names %s, which the enum does not declare", n)
				continue
			}
			out = append(out, v)
		}
		sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
		return out
	}
	wantDM, wantAlways := toNums(dmNames), toNums(alwaysNames)

	gotDM, gotAlways := gm.IgnoredStatuses()
	sort.Slice(gotDM, func(i, j int) bool { return gotDM[i] < gotDM[j] })
	sort.Slice(gotAlways, func(i, j int) bool { return gotAlways[i] < gotAlways[j] })

	if fmt.Sprint(gotDM) != fmt.Sprint(wantDM) {
		t.Errorf("the DM-only ignore set is %v at the pin, Agent GM carries %v", wantDM, gotDM)
	}
	if fmt.Sprint(gotAlways) != fmt.Sprint(wantAlways) {
		t.Errorf("the always-ignore set is %v at the pin, Agent GM carries %v", wantAlways, gotAlways)
	}
}

// Assertion 11: sendRetryBackoff is still [3s, 8s, 20s] and
// isTransientSendFailure still names only FAILURE_2 and FAILURE_3.
func TestAssertion11_SendRetry(t *testing.T) {
	dir := upstreamDir(t)
	src := read(t, dir, "pkg", "connector", "handlematrix.go")
	mustContain(t, src,
		"var sendRetryBackoff = []time.Duration{3 * time.Second, 8 * time.Second, 20 * time.Second}",
		"sendRetryBackoff")
	idx := strings.Index(src, "func isTransientSendFailure(")
	if idx < 0 {
		t.Fatal("isTransientSendFailure not found")
	}
	body := src[idx : idx+400]
	mustContain(t, body, "gmproto.SendMessageResponse_FAILURE_2, gmproto.SendMessageResponse_FAILURE_3", "isTransientSendFailure")
	for _, bad := range []string{"FAILURE_4", "SendMessageResponse_UNKNOWN"} {
		if strings.Contains(body, bad) {
			t.Errorf("isTransientSendFailure now names %s; Agent GM retries only FAILURE_2 and FAILURE_3", bad)
		}
	}
	if len(gm.SendRetryBackoff) != 3 ||
		gm.SendRetryBackoff[0].String() != "3s" ||
		gm.SendRetryBackoff[1].String() != "8s" ||
		gm.SendRetryBackoff[2].String() != "20s" {
		t.Errorf("Agent GM's send backoff is %v, the pin says [3s 8s 20s]", gm.SendRetryBackoff)
	}
	if !gm.SendStatusFailure2.IsTransient() || !gm.SendStatusFailure3.IsTransient() {
		t.Error("FAILURE_2 and FAILURE_3 must be transient")
	}
	if gm.SendStatusFailure4.IsTransient() {
		t.Error("FAILURE_4 must not be retried: upstream renders it as `not your default SMS app`")
	}
}

// Assertion 13: EmojiType still declares exactly the 14 values of section
// 3.7, Unicode() still renders RED_HEART as the variation-selector form, and
// UnicodeToEmojiType still accepts both spellings.
func TestAssertion13_EmojiType(t *testing.T) {
	dir := upstreamDir(t)
	proto := read(t, dir, "pkg", "libgm", "gmproto", "conversations.proto")
	values := enumValues(t, proto, "EmojiType")
	if len(values) != 14 {
		t.Errorf("EmojiType declares %d values, spec section 3.7 says 14", len(values))
	}
	src := read(t, dir, "pkg", "libgm", "gmproto", "emojitype.go")
	mustContain(t, src, "case EmojiType_RED_HEART:\n\t\treturn \"❤️\"", "Unicode() RED_HEART")
	mustContain(t, src, "case \"❤\", \"❤️\":\n\t\treturn EmojiType_RED_HEART", "UnicodeToEmojiType RED_HEART")

	// Both spellings canonicalise to the same value in Agent GM, so a react_
	// ID cannot differ between the write and the read.
	a, ea := gm.CanonicaliseEmojiInput("❤")
	b, eb := gm.CanonicaliseEmojiInput("❤️")
	if a != b || ea == nil || eb == nil || *ea != *eb {
		t.Errorf("the two heart spellings canonicalise differently: %v/%v", ea, eb)
	}
	if *ea != "❤️" {
		t.Errorf("the canonical heart is %q, upstream renders %q", *ea, "❤️")
	}
	// Every value the pin declares has a name in Agent GM.
	for name, v := range values {
		if _, ok := gm.EmojiTypeForRaw(v); !ok {
			t.Errorf("EmojiType %s(%d) has no name in Agent GM", name, v)
		}
	}
}

// Assertion 14: util.GenerateTmpID still returns a bare uuid.NewString().
func TestAssertion14_GenerateTmpID(t *testing.T) {
	dir := upstreamDir(t)
	src := read(t, dir, "pkg", "libgm", "util", "func.go")
	mustContain(t, src, "return uuid.NewString()", "util.GenerateTmpID")
	mustContain(t, src, "Matches what the native app does", "util.GenerateTmpID comment")
	if _, err := parseUUID(gm.GenerateTmpID()); err != nil {
		t.Errorf("Agent GM's tmp ID is not a bare UUID: %v", err)
	}
}

// Assertion 15: the gaia required-cookie list is still exactly
// SID, HSID, OSID, SSID, APISID, SAPISID, with OSID scoped to
// messages.google.com and the rest to .google.com.
func TestAssertion15_GaiaCookies(t *testing.T) {
	dir := upstreamDir(t)
	src := read(t, dir, "pkg", "connector", "login.go")
	mustContain(t, src,
		`requiredCookies := []string{"SID", "HSID", "OSID", "SSID", "APISID", "SAPISID"}`,
		"required cookie list")
	mustContain(t, src, `"OSID":             "messages.google.com",`, "OSID domain")
	for _, c := range []string{"SID", "HSID", "SSID", "APISID", "SAPISID", "__Secure-1PSIDTS"} {
		if !regexp.MustCompile(`"` + regexp.QuoteMeta(c) + `":\s+"\.google\.com",`).MatchString(src) {
			t.Errorf("%s is no longer scoped to .google.com at the pin", c)
		}
	}
	mustContain(t, src,
		"https://accounts.google.com/AccountChooser?continue=https://messages.google.com/web/config",
		"capture URL")

	want := []string{"SID", "HSID", "OSID", "SSID", "APISID", "SAPISID"}
	got := append([]string(nil), gm.GaiaRequiredCookies...)
	sort.Strings(want)
	sortedGot := append([]string(nil), got...)
	sort.Strings(sortedGot)
	if fmt.Sprint(want) != fmt.Sprint(sortedGot) {
		t.Errorf("Agent GM requires %v, the pin requires %v", got, want)
	}
	if gm.GaiaCookieDomains["OSID"] != "messages.google.com" {
		t.Error("Agent GM must scope OSID to messages.google.com")
	}
	if gm.GaiaCaptureURL != "https://accounts.google.com/AccountChooser?continue=https://messages.google.com/web/config" {
		t.Errorf("Agent GM's capture URL is %q", gm.GaiaCaptureURL)
	}
}

// Assertion 16: StartGaiaPairing still selects by last-seen and
// GaiaHackyDeviceSwitcher rather than erroring on several devices, and
// ErrHadMultipleDevices still appears only wrapped in ErrPairingInitTimeout.
func TestAssertion16_DeviceSelection(t *testing.T) {
	dir := upstreamDir(t)
	src := read(t, dir, "pkg", "libgm", "pair_google.go")
	mustContain(t, src, "return b.LastSeen.Compare(a.LastSeen)", "newest-first sort")
	mustContain(t, src, "destRegDev := primaryDevices[c.GaiaHackyDeviceSwitcher%len(primaryDevices)]", "device switcher")
	mustContain(t, src, "return \"\", nil, ErrNoDevicesFound", "zero devices")

	// ErrHadMultipleDevices appears exactly once outside its declaration, and
	// that one use wraps it inside ErrPairingInitTimeout.
	uses := regexp.MustCompile(`ErrHadMultipleDevices`).FindAllStringIndex(src, -1)
	if len(uses) != 2 {
		t.Errorf("ErrHadMultipleDevices appears %d times in pair_google.go, expected its declaration plus one wrapped use", len(uses))
	}
	mustContain(t, src, `err = fmt.Errorf("%w (%w)", ErrPairingInitTimeout, ErrHadMultipleDevices)`,
		"ErrHadMultipleDevices wrapping")
}

// Assertion 17: the listen loop still makes 401 and 403 fatal.
func TestAssertion17_FatalListenStatuses(t *testing.T) {
	dir := upstreamDir(t)
	src := read(t, dir, "pkg", "libgm", "longpoll.go")
	mustContain(t, src, "http.StatusUnauthorized", "401 fatal")
	mustContain(t, src, "http.StatusForbidden", "403 fatal")
	mustContain(t, read(t, dir, "pkg", "libgm", "events", "ready.go"),
		`return fmt.Sprintf("http %d while %s", he.Resp.StatusCode, he.Action)`,
		"HTTPError renders both the same way")

	// Agent GM matches on the value, never the string, so a 403 does not fall
	// into the retry branch and loop forever on dead credentials.
	if !gm.IsFatalListenError(gm.HTTPError{Action: "polling", StatusCode: 401}) {
		t.Error("401 must be fatal")
	}
	if !gm.IsFatalListenError(gm.HTTPError{Action: "polling", StatusCode: 403}) {
		t.Error("403 must be fatal")
	}
	if gm.IsFatalListenError(gm.HTTPError{Action: "polling", StatusCode: 500}) {
		t.Error("500 must not be fatal")
	}
}

// Assertion 18: deduplicateUpdate's callers still `return` out of the batch
// loop on a hit -- the loss behaviour the reconciliation sweep exists for.
func TestAssertion18_DedupAbandonsBatch(t *testing.T) {
	dir := upstreamDir(t)
	src := read(t, dir, "pkg", "libgm", "event_handler.go")
	hits := regexp.MustCompile(`if c\.deduplicateUpdate\(.*\) \{\n\s*return\n`).FindAllString(src, -1)
	if len(hits) != 2 {
		t.Errorf("expected two callers of deduplicateUpdate that return out of the batch loop, found %d", len(hits))
	}
	mustContain(t, read(t, dir, "pkg", "libgm", "client.go"), "recentUpdates    [8]updateDedupItem", "8-entry dedup window")
}

// Assertion 19: events.NewBrowserActive still has no callers outside
// pkg/libgm/gmtest, so section 3.4 is right not to subscribe to it.
func TestAssertion19_BrowserActiveHasNoEmitters(t *testing.T) {
	dir := upstreamDir(t)
	hits := grepTree(t, dir, regexp.MustCompile(`NewBrowserActive\(`), "pkg/libgm/events/useralerts.go", "pkg/libgm/gmtest")
	if len(hits) != 0 {
		t.Errorf("events.NewBrowserActive now has callers: %v -- spec section 3.4 must be revisited", hits)
	}
	// events.QR is dead at this pin too (D19): it is only ever a return value.
	qr := grepTree(t, dir, regexp.MustCompile(`events\.QR\{`))
	if len(qr) != 0 {
		t.Errorf("events.QR now has emitters: %v -- D19 must be revisited", qr)
	}
}

// Assertion 20: connector/login.go still re-authenticates an existing pairing
// from fresh cookies, gated on a non-nil tachyon token and a non-nil
// PairingID. Cookie expiry therefore does not force a re-pair.
func TestAssertion20_CookieRefreshWithoutRepair(t *testing.T) {
	dir := upstreamDir(t)
	src := read(t, dir, "pkg", "connector", "login.go")
	mustContain(t, src, "Session.TachyonAuthToken != nil && ", "re-auth gate (tachyon token)")
	mustContain(t, src, "PairingID != uuid.Nil", "re-auth gate (pairing ID)")
	mustContain(t, src, "GetDeviceInfo().GetEmail()", "same-account verification")
	mustContain(t, src, "SetCookies(nil)", "restore on failure")
}

func assertEnum(t *testing.T, what string, got, want map[string]int32) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s declares %d values (%v), spec says %d", what, len(got), got, len(want))
	}
	for name, v := range want {
		g, ok := got[name]
		if !ok {
			t.Errorf("%s: %s is gone from the pin", what, name)
			continue
		}
		if g != v {
			t.Errorf("%s: %s is %d at the pin, spec says %d", what, name, g, v)
		}
	}
	for name := range got {
		if _, ok := want[name]; !ok {
			t.Errorf("%s: the pin declares %s=%d, which the spec does not name", what, name, got[name])
		}
	}
}

var uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func parseUUID(s string) (string, error) {
	if !uuidRe.MatchString(s) {
		return "", fmt.Errorf("%q is not a bare UUID", s)
	}
	return s, nil
}
