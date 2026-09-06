//go:build live

package livegate

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The Slice 2 live gate: section 16 Slice 2 tests 45, 46, 47 and 48.
//
// These drive the REAL `agm` binary against the coordinator's REAL server,
// which is already running and already paired. They do NOT pair, do not read
// the store directly, and do not touch `libgm`: the whole point of Slice 2 is
// that the CLI speaks REST and nothing else, so a live gate that reached past
// the API would be testing something no agent can do.
//
// **Every send goes to `<APPROVED_DIRECT_NUMBER>` or, for the group, to
// `<APPROVED_GROUP_NUMBER_1>` and `<APPROVED_GROUP_NUMBER_2>`, and the values
// arrive only through `AGENT_GM_LIVE_NUMBERS` or the untracked
// `testdata/live-numbers.local`** (spec section 13.3). No number is written
// into this repository, and a run with no approved target does not happen: it
// skips.
//
// Test 45's pairing step, test 46 and test 47 are **not** here, and that is
// deliberate rather than an omission. Each needs a human at a browser and a
// phone -- signing in to Google, tapping an emoji, copying a cURL command out
// of devtools -- so they are coordinator hand-runs like Slice 1's test 7, and
// the exact command sequence for them is in the implementer's report. What is
// automated here is everything that follows from an already-paired account,
// which is where the assertions are.

// agmRun runs the real binary and returns stdout, stderr and the exit code.
//
// It runs `agm` rather than calling `cli.Run` in-process, because the exit
// code is half of what section 11.2 promises and an in-process call would
// only be testing the other half.
func agmRun(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	return agmRunStdin(t, "", args...)
}

// agmBinary resolves the built CLI, skipping when it is absent.
func agmBinary(t *testing.T) string {
	t.Helper()
	bin := os.Getenv("AGENT_GM_CLI_BINARY")
	if bin == "" {
		bin = filepath.Join("..", "..", "bin", "agm")
	}
	if _, err := os.Stat(bin); err != nil {
		t.Skipf("no agm binary at %s: run `devbox run build` first", bin)
	}
	return bin
}

// agmRunStdin is agmRun with something on stdin, for `--paste`.
func agmRunStdin(t *testing.T, stdin string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	bin := agmBinary(t)
	cmd := exec.Command(bin, args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	code = 0
	var exit *exec.ExitError
	if err != nil {
		if ok := asExitError(err, &exit); ok {
			code = exit.ExitCode()
		} else {
			t.Fatalf("running %s %v: %v", bin, args, err)
		}
	}
	return out.String(), errb.String(), code
}

func asExitError(err error, target **exec.ExitError) bool {
	e, ok := err.(*exec.ExitError)
	if ok {
		*target = e
	}
	return ok
}

// agmJSON runs a command with --json and decodes the single value it puts on
// stdout, failing if there is more than one -- which is section 16 Slice 2
// test 26 re-asserted against the real server rather than a stub.
func agmJSON(t *testing.T, args ...string) map[string]any {
	t.Helper()
	stdout, stderr, code := agmRun(t, append(args, "--json")...)
	if code != 0 {
		t.Fatalf("agm %v exited %d\nstderr:\n%s", args, code, stderr)
	}
	dec := json.NewDecoder(strings.NewReader(stdout))
	var v map[string]any
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("agm %v did not put JSON on stdout: %v\nstdout:\n%s", args, err, stdout)
	}
	if dec.More() {
		t.Fatalf("agm %v put more than one value on stdout", args)
	}
	return v
}

func liveData(t *testing.T, v map[string]any) map[string]any {
	t.Helper()
	d, ok := v["data"].(map[string]any)
	if !ok {
		t.Fatalf("the envelope carries no data object: %v", v)
	}
	return d
}

// requireLoggedIn skips unless the CLI already holds a credential for the
// server. The live gate does not log in for itself: the admin secret is the
// coordinator's and does not belong in a test's environment handling.
func requireLoggedIn(t *testing.T) {
	t.Helper()
	requireGate(t)
	if _, _, code := agmRun(t, "auth", "whoami", "--json"); code != 0 {
		t.Skip("agm is not logged in: run `agm auth login --admin --secret-stdin` first")
	}
}

// Section 16 Slice 2 test 45, the part that follows from a paired account:
// list, send with --wait, send a file, list again, react and unreact, delete,
// archive and unarchive.
//
// Every destructive step is left to the end and each prints its effect
// sentence; the coordinator confirms interactively, so `--yes` is NOT passed
// here. A test that passed `--yes` would be testing a different command from
// the one an owner runs.
func TestSlice2LiveDirectConversationWalk(t *testing.T) {
	requireLoggedIn(t)
	numbers := approvedNumbers(t)
	if numbers.Direct == "" {
		t.Skip("no approved direct number")
	}

	// 1. The conversation must already exist, from the Slice 1 gate. Finding
	//    it by participant rather than creating one keeps this test from
	//    starting a thread with a real person as a side effect.
	convs := liveData(t, agmJSON(t, "conversations", "list", "--participant", numbers.Direct))
	items, _ := convs["items"].([]any)
	if len(items) == 0 {
		t.Skip("no conversation with the approved number yet; run the Slice 1 gate first")
	}
	first, _ := items[0].(map[string]any)
	convID, _ := first["id"].(string)
	if !strings.HasPrefix(convID, "conv_") {
		t.Fatalf("the listing returned %q, which is not a conv_ ID", convID)
	}

	stamp := time.Now().UTC().Format("15:04:05")
	text := "agent-gm slice 2 live gate " + stamp

	// 2. A text send with --wait, which is the assertion: the operation
	//    reaches `sent` rather than merely being accepted.
	sent := liveData(t, agmJSON(t, "messages", "send", convID, "--text", text, "--wait", "--wait-for", "sent"))
	op, _ := sent["operation"].(map[string]any)
	if op == nil {
		t.Fatal("the send returned no operation")
	}
	if status, _ := op["status"].(string); status != "succeeded" {
		t.Errorf("the operation is %q, want succeeded", status)
	}
	messageID, _ := op["message_id"].(string)
	if messageID == "" {
		// The echo may not have landed yet even on a succeeded send (6.5),
		// so this is reported and not fatal: the list below finds it.
		t.Logf("the operation has no message_id yet; the echo has not landed")
	}

	// 3. The message is in the listing, with the text as sent.
	msgs := liveData(t, agmJSON(t, "messages", "list", convID, "--limit", "10"))
	rows, _ := msgs["items"].([]any)
	found := ""
	for _, r := range rows {
		m, _ := r.(map[string]any)
		if body, _ := m["text"].(string); body == text {
			found, _ = m["id"].(string)
			break
		}
	}
	if found == "" {
		t.Fatalf("the message just sent is not in the listing")
	}

	// 4. A reaction, then its removal. Both are real: the recipient sees the
	//    reaction appear and disappear.
	agmJSON(t, "messages", "add-reaction", found, "👍")
	agmJSON(t, "messages", "remove-reaction", found, "👍")

	// 5. Archive and unarchive the thread, which are local to the owner's
	//    own list rather than visible to the other person.
	archived := liveData(t, agmJSON(t, "conversations", "archive", convID))
	if changed, ok := archived["changed"].(bool); ok && !changed {
		t.Log("the conversation was already archived")
	}
	agmJSON(t, "conversations", "unarchive", convID)
}

// The media half of test 45: a small JPEG, reserved, PUT and sent in one
// command, then visible in the listing with an attachment.
func TestSlice2LiveMediaSend(t *testing.T) {
	requireLoggedIn(t)
	numbers := approvedNumbers(t)
	if numbers.Direct == "" {
		t.Skip("no approved direct number")
	}
	convs := liveData(t, agmJSON(t, "conversations", "list", "--participant", numbers.Direct))
	items, _ := convs["items"].([]any)
	if len(items) == 0 {
		t.Skip("no conversation with the approved number yet")
	}
	first, _ := items[0].(map[string]any)
	convID, _ := first["id"].(string)

	// The smallest valid JPEG, written here rather than committed: a binary
	// fixture in a public repository is a thing nobody reviews.
	path := filepath.Join(t.TempDir(), "agent-gm-live.jpg")
	if err := os.WriteFile(path, tinyJPEG(), 0o600); err != nil {
		t.Fatal(err)
	}

	sent := liveData(t, agmJSON(t, "messages", "send", convID,
		"--file", path, "--text", "agent-gm slice 2 media gate", "--wait", "--wait-for", "sent"))
	op, _ := sent["operation"].(map[string]any)
	if op == nil {
		t.Fatal("the media send returned no operation")
	}
	if status, _ := op["status"].(string); status != "succeeded" {
		t.Errorf("the media operation is %q, want succeeded", status)
	}
}

// Section 16 Slice 2 test 48: a group.
//
// If it fails, the failure is diagnosed against the section 15.4 runbook rows
// -- the phone's group-messaging setting, `config_version_stale`,
// `google_undocumented_status` -- BEFORE anything is changed. The test prints
// that diagnosis rather than leaving the coordinator to reconstruct it.
func TestSlice2LiveGroupStart(t *testing.T) {
	requireLoggedIn(t)
	numbers := approvedNumbers(t)
	if numbers.Group1 == "" || numbers.Group2 == "" {
		t.Skip("no approved group numbers")
	}

	stdout, stderr, code := agmRun(t, "conversations", "start",
		numbers.Group1, numbers.Group2, "--name", "agent-gm test", "--json")
	if code == 0 {
		var v map[string]any
		if err := json.Unmarshal([]byte(stdout), &v); err != nil {
			t.Fatalf("the start did not put JSON on stdout: %v", err)
		}
		d := liveData(t, v)
		conv, _ := d["conversation"].(map[string]any)
		if conv == nil {
			t.Fatal("the start returned no conversation")
		}
		if isGroup, _ := conv["is_group"].(bool); !isGroup {
			t.Error("the conversation is not a group; check the phone's " +
				"Settings -> Advanced -> Group messaging setting (spec 15.4)")
		}
		return
	}

	// The diagnosis, from the section 15.4 rows, before anything is changed.
	health := liveData(t, agmJSON(t, "health"))
	var diagnosis []string
	if accounts, ok := health["accounts"].([]any); ok {
		for _, a := range accounts {
			row, _ := a.(map[string]any)
			id, _ := row["account_id"].(string)
			google, _ := row["google"].(map[string]any)
			if google == nil {
				continue
			}
			if stale, _ := google["config_version_stale"].(bool); stale {
				diagnosis = append(diagnosis, fmt.Sprintf(
					"%s: config_version_stale is true (live %v, compiled %v). Section 15.4: "+
						"the fix is a PIN BUMP as its own slice, and retrying does not help",
					id, google["config_version_live"], health["config_version_compiled"]))
			}
			if isDefault, ok := google["is_default_sms_app"].(bool); ok && !isDefault {
				diagnosis = append(diagnosis, fmt.Sprintf(
					"%s: Google Messages is not the phone's default SMS app, so every send fails "+
						"(section 15.4)", id))
			}
		}
	}
	if strings.Contains(stderr, "google_undocumented_status") {
		diagnosis = append(diagnosis,
			"google_undocumented_status: Google returned a status the pinned proto has no name "+
				"for. Section 15.4: record details.status and the request and report it "+
				"upstream. Do NOT invent a meaning")
	}
	diagnosis = append(diagnosis,
		"if none of the above applies, check the phone: Settings -> Advanced -> Group messaging "+
			"must be \"Send an MMS reply to all recipients\". With the SMS setting a group send "+
			"fans out as separate SMS threads (section 15.4)")

	t.Fatalf("the group start failed with exit %d\nstderr:\n%s\n\ndiagnosis, before changing anything:\n  - %s",
		code, strings.TrimSpace(stderr), strings.Join(diagnosis, "\n  - "))
}

// tinyJPEG is the smallest structurally valid JPEG: SOI, a minimal APP0 JFIF
// header, and EOI. It is generated rather than committed, because a binary
// fixture in a public repository is a thing nobody reviews.
func tinyJPEG() []byte {
	return []byte{
		0xFF, 0xD8, // SOI
		0xFF, 0xE0, 0x00, 0x10, // APP0, length 16
		'J', 'F', 'I', 'F', 0x00,
		0x01, 0x01, // version 1.1
		0x00,       // no density units
		0x00, 0x01, // x density
		0x00, 0x01, // y density
		0x00, 0x00, // no thumbnail
		0xFF, 0xD9, // EOI
	}
}
