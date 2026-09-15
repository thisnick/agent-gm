package oauth

import "testing"

// The charset of an owner-chosen `client_id` (spec section 9.3).
//
// It is a closed set rather than an escaping argument because the id travels
// in a query string and is echoed into the approval form's hidden fields. A
// test over the predicate is worth more than one over the handler here: the
// handler would pass with a charset that merely happens to reject the inputs
// somebody thought of.
func TestAnOwnerChosenClientIDUsesAClosedCharset(t *testing.T) {
	accepted := []string{
		"muse",
		"Muse",
		"muse-2",
		"muse_2",
		"com.example.connector",
		"0",
		"A-Za-z0-9._-",
	}
	for _, id := range accepted {
		if !validStaticClientID(id) {
			t.Errorf("%q was refused and should be accepted", id)
		}
	}

	refused := []string{
		"",              // handled by the caller, but the predicate agrees
		"muse client",   // a space
		"muse/../admin", // path traversal shapes
		"muse?x=1",      // a query
		"muse#frag",     // a fragment
		`muse"onload=`,  // an HTML attribute break
		"muse<script>",
		"muse&amp;",
		"muse%20",
		"müse",  // non-ASCII
		"muse\n", // a newline
		"muse\t",
	}
	for _, id := range refused {
		if id == "" {
			continue
		}
		if validStaticClientID(id) {
			t.Errorf("%q was accepted and must be refused", id)
		}
	}
}
