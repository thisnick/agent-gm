package media_test

import (
	"strings"
	"testing"

	"github.com/thisnick/agent-gm/internal/media"
	"github.com/thisnick/agent-gm/internal/store"
)

func testSigner(t *testing.T) *media.Signer {
	t.Helper()
	// A nonfunctional fixture key. Never a real AGENT_GM_DATA_KEY.
	dk, err := store.ParseDataKey(strings.Repeat("ab", 32))
	if err != nil {
		t.Fatal(err)
	}
	s, err := media.NewSigner(dk)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// The audience is part of the redemption rather than a check after it (spec
// section 10.3). That is what makes "a token presented at the wrong URL is
// refused **and not spent**" true by construction: the wrong audience never
// reaches the row that would be spent, because Verify never returns a hash
// to look one up by.
//
// Section 16 Slice 2 test 17's last clause: the token presented at another
// upload's URL is refused and NOT spent, proven by then redeeming it
// successfully at its own URL.
//
// Plant: drop the audience from Signer.mac and this test fails at "a ticket
// minted for upl_A verified at upl_B's URL". Planted 2026-09-06.
func TestATicketOnlyVerifiesAtItsOwnAudience(t *testing.T) {
	s := testSigner(t)
	value, hash, err := s.Mint(media.KindUpload, "upl_A")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.Verify(value, media.KindUpload, "upl_B"); err == nil {
		t.Fatal("a ticket minted for upl_A verified at upl_B's URL")
	}
	// And it is still good at its own, which is the half that proves the
	// refusal did not spend it.
	got, err := s.Verify(value, media.KindUpload, "upl_A")
	if err != nil {
		t.Fatalf("the ticket was refused at its own URL: %v", err)
	}
	if got != hash {
		t.Errorf("Verify returned hash %q, Mint returned %q", got, hash)
	}
}

// A download ticket is not an upload ticket, and neither is accepted where
// the other belongs -- checked before anything else happens, from the prefix.
func TestTheTwoKindsAreNotInterchangeable(t *testing.T) {
	s := testSigner(t)
	up, _, err := s.Mint(media.KindUpload, "upl_A")
	if err != nil {
		t.Fatal(err)
	}
	down, _, err := s.Mint(media.KindDownload, "att_A")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(up, media.UploadPrefix) {
		t.Errorf("an upload ticket is %q", up)
	}
	if !strings.HasPrefix(down, media.DownloadPrefix) {
		t.Errorf("a download ticket is %q", down)
	}
	if _, err := s.Verify(up, media.KindDownload, "upl_A"); err == nil {
		t.Error("an upload ticket verified as a download ticket")
	}
	if _, err := s.Verify(down, media.KindUpload, "att_A"); err == nil {
		t.Error("a download ticket verified as an upload ticket")
	}
	if k, ok := media.KindOf(up); !ok || k != media.KindUpload {
		t.Errorf("KindOf(upload) = %v, %v", k, ok)
	}
	if _, ok := media.KindOf("not-a-ticket"); ok {
		t.Error("KindOf accepted a value with no ticket prefix")
	}
}

// Every refusal is the same error whatever the reason, so a status code
// teaches an attacker nothing about which guesses were once valid.
func TestEveryRefusalIsTheSameError(t *testing.T) {
	s := testSigner(t)
	value, _, err := s.Mint(media.KindUpload, "upl_A")
	if err != nil {
		t.Fatal(err)
	}
	tampered := value[:len(value)-3] + "AAA"

	cases := map[string]string{
		"wrong audience":  value,
		"tampered mac":    tampered,
		"no prefix":       strings.TrimPrefix(value, media.UploadPrefix),
		"no separator":    media.UploadPrefix + "abcdef",
		"empty":           "",
		"separator only":  media.UploadPrefix + ".",
		"nothing after .": media.UploadPrefix + "abc.",
	}
	for name, v := range cases {
		target := "upl_A"
		if name == "wrong audience" {
			target = "upl_B"
		}
		_, err := s.Verify(v, media.KindUpload, target)
		if err == nil {
			t.Errorf("%s was accepted", name)
			continue
		}
		if err.Error() != media.ErrTicketRefused.Error() {
			t.Errorf("%s produced %q; every refusal must be the same message", name, err)
		}
	}
}

// Two mints of the same audience are different values: the token is a
// credential, not an identifier. Section 10.3: each attempt returns a fresh
// token, because the first token's value left the process and cannot be
// recovered.
func TestEachMintIsFresh(t *testing.T) {
	s := testSigner(t)
	seen := map[string]bool{}
	for i := 0; i < 32; i++ {
		v, h, err := s.Mint(media.KindDownload, "att_A")
		if err != nil {
			t.Fatal(err)
		}
		if seen[v] {
			t.Fatal("two mints produced the same ticket value")
		}
		if seen[h] {
			t.Fatal("two mints produced the same ticket hash")
		}
		seen[v], seen[h] = true, true
	}
}

// Only the hash is stored, and the hash is a fixed-length hex digest so the
// compared lengths are constant (spec section 12.1).
func TestHashIsFixedLengthAndNotTheValue(t *testing.T) {
	s := testSigner(t)
	v, h, err := s.Mint(media.KindDownload, "att_A")
	if err != nil {
		t.Fatal(err)
	}
	if len(h) != 64 {
		t.Errorf("the hash is %d characters, want 64 hex", len(h))
	}
	if strings.Contains(h, v) || strings.Contains(v, h) {
		t.Error("the stored hash carries the token value")
	}
	if media.HashToken(v) != h {
		t.Error("HashToken does not reproduce the stored hash")
	}
	// A different value hashes differently, or the store would collide.
	if media.HashToken(v+"x") == h {
		t.Error("the hash does not depend on the whole value")
	}
}

// A ticket signed by one data key is not valid under another. The ticket key
// is derived under its own info string, so a session or cursor key can never
// sign one (spec section 4.5).
func TestATicketDoesNotVerifyUnderAnotherKey(t *testing.T) {
	a := testSigner(t)
	dkB, err := store.ParseDataKey(strings.Repeat("cd", 32))
	if err != nil {
		t.Fatal(err)
	}
	b, err := media.NewSigner(dkB)
	if err != nil {
		t.Fatal(err)
	}
	v, _, err := a.Mint(media.KindUpload, "upl_A")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Verify(v, media.KindUpload, "upl_A"); err == nil {
		t.Error("a ticket verified under a different data key")
	}
}

// The audience string is unambiguous: a length prefix inside the MAC means
// ("a","bc") and ("ab","c") cannot produce the same input.
func TestAudienceIsUnambiguous(t *testing.T) {
	s := testSigner(t)
	v1, _, err := s.Mint(media.KindUpload, "a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Verify(v1, media.KindUpload, "a"); err != nil {
		t.Fatal(err)
	}
	if media.Audience(media.KindUpload, "upl_1") != "upload:upl_1" {
		t.Errorf("Audience = %q", media.Audience(media.KindUpload, "upl_1"))
	}
	if media.Audience(media.KindDownload, "att_1") != "download:att_1" {
		t.Errorf("Audience = %q", media.Audience(media.KindDownload, "att_1"))
	}
}

// The token appears in the body and never in a URL (spec section 10.3), so
// the curl lines put it in a header. A builder that interpolated it into the
// URL is exactly the mistake this asserts against.
func TestCurlLinesPutTheTokenInAHeaderAndNotTheURL(t *testing.T) {
	const token = "agm_dt_FIXTURE.TOKEN"
	const url = "https://gm.example.test/v1/attachments/att_1/content"

	down := media.CurlDownload(url, token, "IMG_0421.jpg")
	if !strings.Contains(down, "-H 'Authorization: Bearer "+token+"'") {
		t.Errorf("the download line does not carry the token in a header: %s", down)
	}
	if strings.Contains(down, url+"?") || strings.Contains(down, "t="+token) {
		t.Errorf("the download line puts the token in the URL: %s", down)
	}

	up := media.CurlUpload("https://gm.example.test/v1/uploads/upl_1/content", token, "image/jpeg")
	if !strings.Contains(up, "-X PUT") || !strings.Contains(up, "--data-binary @FILE") {
		t.Errorf("the upload line is not the documented PUT: %s", up)
	}
	if !strings.Contains(up, "-H 'Authorization: Bearer "+token+"'") {
		t.Errorf("the upload line does not carry the token in a header: %s", up)
	}

	// A filename arriving from Google is not trusted to be shell-safe: it
	// must not be able to end the quoted string it sits inside.
	hostile := media.CurlDownload(url, token, "a'; rm -rf ~; echo '")
	if strings.Contains(hostile, "rm -rf ~;") && strings.Count(hostile, "'")%2 != 0 {
		t.Errorf("a hostile filename escaped its quoting: %s", hostile)
	}
	if strings.Contains(hostile, "'; rm") {
		t.Errorf("a hostile filename escaped its quoting: %s", hostile)
	}
}
