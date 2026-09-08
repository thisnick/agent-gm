package main

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestOAuthCompletionRefreshDoesNotIssueCode(t *testing.T) {
	h := newOAuthHarness(t)
	admin := h.adminToken()
	id, form := h.pendingRequest(admin, "http://127.0.0.1:53217/callback")
	path := "/oauth/requests/" + id + "/complete"
	for _, state := range []string{"pending", "approved"} {
		if state == "approved" {
			resp := h.postJSON("/v1/admin/authorization-requests/"+id+"/approve", map[string]any{}, bearer(admin))
			if resp.StatusCode != http.StatusOK {
				t.Fatal(readBody(t, resp))
			}
			_ = readBody(t, resp)
		}
		resp := h.get(path, h.withCookie)
		body := readBody(t, resp)
		if resp.StatusCode != http.StatusOK || !strings.Contains(body, `data-status="`+state+`"`) {
			t.Fatalf("refresh: %d %s", resp.StatusCode, body)
		}
		if resp.Header.Get("Location") != "" {
			t.Fatal("GET must not issue a code or redirect to the client")
		}
	}
	resp := h.get(path)
	_ = readBody(t, resp)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatal("refresh without cookie disclosed request")
	}
	// A GET after approval must leave the single-use POST available.
	resp = h.postForm(path, url.Values{"form_token": {form.Get("form_token")}}, h.withCookie, h.sameOrigin)
	_ = readBody(t, resp)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST after refresh: %d", resp.StatusCode)
	}
}
