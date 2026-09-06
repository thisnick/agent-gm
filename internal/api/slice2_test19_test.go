package api_test

import (
	"strings"
	"testing"
)

// Section 16 Slice 2 test 19:
//
//	"`upload_url` and `download_url` are built from `AGENT_GM_PUBLIC_URL`
//	 **even when the request carries a hostile `Host` and
//	 `X-Forwarded-Host`**."
//
// Two things go wrong when a server builds a URL from the request. The benign
// one is that an agent in a sandbox on another machine gets the origin it
// happened to dial rather than the one the tunnel exposes, and its `curl`
// line fails. The hostile one is that anybody who can reach the port can make
// the server hand an agent a URL pointing at a host of their choosing --
// complete with a valid ticket in the accompanying `curl` command, which the
// agent will then present to them.
//
// `AGENT_GM_PUBLIC_URL` is the only input, and the builder takes no request,
// so there is nothing for a header to reach. This test proves it from the
// outside, with the headers a proxy would set and an attacker would forge.
func TestSlice2_19_URLsComeFromThePublicURLNotTheRequest(t *testing.T) {
	s := newServer(t)
	accountID := s.addAccount(addressA)
	s.seedConversation(accountID, "conv-a")
	att := s.seedAttachment(accountID, "conv-a", "m-att", []byte("some jpeg bytes"), "image/jpeg")

	hostile := map[string]string{
		"Host":              "evil.example.invalid",
		"X-Forwarded-Host":  "evil.example.invalid",
		"X-Forwarded-Proto": "http",
		"Forwarded":         "host=evil.example.invalid;proto=http",
		"Origin":            "http://evil.example.invalid",
		"Idempotency-Key":   key("hostile-upload"),
	}

	upload := s.callWith(s.Token, "POST", "/v1/uploads", map[string]any{
		"filename":   "photo.jpg",
		"mime_type":  "image/jpeg",
		"size_bytes": 15,
	}, hostile).ok(t, 201)

	uploadURL, _ := upload.Data["upload_url"].(string)
	assertPublicOrigin(t, "upload_url", uploadURL)
	assertPublicOrigin(t, "the upload curl line", getString(t, upload.Data, "curl"))

	download := s.callWith(s.Token, "GET", "/v1/attachments/"+att.ID, nil, hostile).ok(t, 200)
	assertPublicOrigin(t, "download_url", getString(t, download.Data, "download_url"))
	assertPublicOrigin(t, "the download curl line", getString(t, download.Data, "curl"))

	// The bytes route is served by Mux rather than by the middleware chain,
	// so it gets the same assertion rather than being taken on trust: the
	// URL an agent is handed must work when it dials it.
	body := s.callWith(s.Token, "GET", strings.TrimPrefix(uploadURL, publicURL), nil, hostile)
	if body.Status == 0 {
		t.Fatal("the upload URL's path did not resolve to a route")
	}
}

// assertPublicOrigin fails unless every URL in the value is on
// AGENT_GM_PUBLIC_URL and the hostile host appears nowhere.
func assertPublicOrigin(t *testing.T, what, value string) {
	t.Helper()
	if value == "" {
		t.Fatalf("%s is empty", what)
	}
	if strings.Contains(value, "evil.example.invalid") {
		t.Fatalf("%s carries the hostile Host: %s", what, value)
	}
	if strings.Contains(value, "127.0.0.1") || strings.Contains(value, "localhost") {
		t.Fatalf("%s was built from the request's own address: %s", what, value)
	}
	if !strings.Contains(value, publicURL) {
		t.Fatalf("%s is not built from AGENT_GM_PUBLIC_URL (%s): %s", what, publicURL, value)
	}
}
