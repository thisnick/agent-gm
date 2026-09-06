package apierr

import (
	"strings"
)

// AuthRealm is the realm every 401 and 403 advertises (spec section 7.2).
const AuthRealm = "agent-gm"

// ResourceMetadataPath is the path the resource_metadata parameter points at.
// It is the protected-resource metadata for the MCP surface, which is what an
// MCP client fetches to discover where to authorize (RFC 9728, spec 9.2).
const ResourceMetadataPath = "/.well-known/oauth-protected-resource/mcp"

// ResourceMetadataURL is the absolute metadata URL for a deployment's public
// URL. It is built from the configured public URL and never from the request
// Host, so a hostile Host or X-Forwarded-Host cannot point a client at an
// attacker's authorization server.
func ResourceMetadataURL(publicURL string) string {
	return strings.TrimRight(publicURL, "/") + ResourceMetadataPath
}

// WWWAuthenticate builds the header value that accompanies a 401 or a 403
// (spec section 7.2): realm="agent-gm", an error parameter, resource_metadata
// pointing at the protected-resource document, and, for a scope refusal, the
// scope the route requires.
//
// requiredScope is used only for insufficient_scope and is ignored otherwise;
// a 401 does not know what scope the caller would have needed, because it has
// not established who the caller is.
func WWWAuthenticate(publicURL string, code Code, requiredScope string) string {
	params := []string{
		`realm="` + quote(AuthRealm) + `"`,
		`error="` + quote(string(code)) + `"`,
	}
	if code == CodeInsufficientScope && requiredScope != "" {
		params = append(params, `scope="`+quote(requiredScope)+`"`)
	}
	params = append(params, `resource_metadata="`+quote(ResourceMetadataURL(publicURL))+`"`)
	return "Bearer " + strings.Join(params, ", ")
}

// WWWAuthenticateFor is WWWAuthenticate for an error value. It returns "" for
// any code that is not 401 or 403, so a handler can call it unconditionally
// and an unrelated error never advertises an authorization challenge.
func WWWAuthenticateFor(publicURL string, e *Error) string {
	if e == nil {
		return ""
	}
	switch e.Code {
	case CodeInvalidToken:
		return WWWAuthenticate(publicURL, e.Code, "")
	case CodeInsufficientScope:
		scope, _ := e.Details["scope"].(string)
		return WWWAuthenticate(publicURL, e.Code, scope)
	default:
		return ""
	}
}

// quote escapes the two characters a quoted-string may not carry literally
// (RFC 9110). Every value here is server-controlled, so this is a belt on top
// of braces rather than the only defence.
func quote(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s)
}
