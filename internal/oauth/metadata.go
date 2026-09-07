package oauth

import (
	"encoding/json"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"

	"github.com/thisnick/agent-gm/internal/authz"
)

// The two discovery documents of spec section 9.2.
//
// **Both are built from the configured public URL as a STRING.** `issuer`
// equals AGENT_GM_PUBLIC_URL byte for byte and `resource` is that plus
// `/mcp`, with no trailing-slash drift, and section 16 Slice 3 test 1 asserts
// both as string equality rather than as parsed-URL equivalence. That is not
// pedantry: a client that fetches `https://gm.example.test/` and compares the
// issuer it got back to the one it asked for will reject a mismatch, and two
// URLs that parse the same are not the same bytes.

// ScopesSupported is the three messaging scopes, in the order both documents
// list them. `admin` is not here and never will be: it is issued only by the
// admin bootstrap and is invalid_scope at /oauth/authorize (section 9.7).
var ScopesSupported = []string{
	string(authz.ScopeMessagesRead),
	string(authz.ScopeMessagesWrite),
	string(authz.ScopeMessagesDelete),
}

// SourceURL is the repository this server's source lives in. It is the
// `resource_documentation` of the protected-resource document, and it is the
// same URL section 1.4's AGPL obligation puts in `GET /v1/health`.
const SourceURL = "https://github.com/thisnick/agent-gm"


type authorizationServerDoc struct {
	Issuer                                     string   `json:"issuer"`
	AuthorizationEndpoint                      string   `json:"authorization_endpoint"`
	TokenEndpoint                              string   `json:"token_endpoint"`
	RegistrationEndpoint                       string   `json:"registration_endpoint"`
	RevocationEndpoint                         string   `json:"revocation_endpoint"`
	ResponseTypesSupported                     []string `json:"response_types_supported"`
	GrantTypesSupported                        []string `json:"grant_types_supported"`
	TokenEndpointAuthMethodsSupported          []string `json:"token_endpoint_auth_methods_supported"`
	CodeChallengeMethodsSupported              []string `json:"code_challenge_methods_supported"`
	ScopesSupported                            []string `json:"scopes_supported"`
	AuthorizationResponseISSParameterSupported bool     `json:"authorization_response_iss_parameter_supported"`
}

// ProtectedResourceDocument is the RFC 9728 document, as a value, so that a
// test can assert its fields without going through HTTP and the MCP layer can
// point at the same strings.
func (s *Server) ProtectedResourceDocument() any {
	return s.protectedResourceMetadata()
}

// protectedResourceMetadata is the RFC 9728 document as the official MCP Go
// SDK's own type (decision D36), so that the handler and the accessor cannot
// drift.
//
// It is the SDK's type rather than one of ours because this document is read
// by clients, not by us: an MCP client fetches it to discover where to
// authorize, and `oauthex.ProtectedResourceMetadata` is what the reference
// implementation both writes and parses. Our values are unchanged and still
// byte-exact -- `resource` is AGENT_GM_PUBLIC_URL plus `/mcp` and nothing
// else -- and section 16 Slice 3 test 1 still asserts them as strings.
func (s *Server) protectedResourceMetadata() *oauthex.ProtectedResourceMetadata {
	return &oauthex.ProtectedResourceMetadata{
		Resource:               s.Resource(),
		AuthorizationServers:   []string{s.Issuer()},
		ScopesSupported:        ScopesSupported,
		BearerMethodsSupported: []string{"header"},
		ResourceDocumentation:  SourceURL,
	}
}

// AuthorizationServerDocument is the RFC 8414 document.
//
// `token_endpoint_auth_methods_supported` is `["none"]` and there is no
// client_secret anywhere in this server: public native clients only (section
// 9.3).
func (s *Server) AuthorizationServerDocument() any {
	return authorizationServerDoc{
		Issuer:                                     s.Issuer(),
		AuthorizationEndpoint:                      s.Issuer() + "/oauth/authorize",
		TokenEndpoint:                              s.Issuer() + "/oauth/token",
		RegistrationEndpoint:                       s.Issuer() + "/oauth/register",
		RevocationEndpoint:                         s.Issuer() + "/oauth/revoke",
		ResponseTypesSupported:                     []string{"code"},
		GrantTypesSupported:                        []string{"authorization_code", "refresh_token"},
		TokenEndpointAuthMethodsSupported:          []string{"none"},
		CodeChallengeMethodsSupported:              []string{"S256"},
		ScopesSupported:                            ScopesSupported,
		AuthorizationResponseISSParameterSupported: true,
	}
}

// protectedResource serves the RFC 9728 document through the SDK's own
// handler (decision D36).
//
// The SDK's handler adds the CORS headers RFC 9728 section 3.1 asks for --
// this document is public discovery data and a browser-based client has to be
// able to read it cross-origin. Section 9.9's headers are set first and
// survive, because the SDK's handler sets only `Content-Type` and the three
// `Access-Control-*` values.
func (s *Server) protectedResource(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	auth.ProtectedResourceMetadataHandler(s.protectedResourceMetadata()).ServeHTTP(w, r)
}

func (s *Server) authorizationServer(w http.ResponseWriter, _ *http.Request) {
	s.writeJSON(w, http.StatusOK, s.AuthorizationServerDocument())
}

func (s *Server) writeJSON(w http.ResponseWriter, status int, body any) {
	setSecurityHeaders(w)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
