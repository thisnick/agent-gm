package oauth

import (
	"encoding/json"
	"net/http"

	"github.com/thisnick/agent-gm/internal/authz"
)

// The two discovery documents of spec section 9.2.
//
// **Both are built from the configured public URL as a STRING.** `issuer`
// equals AGENT_GM_PUBLIC_URL byte for byte and `resource` is that plus
// `/mcp`, with no trailing-slash drift, and section 16 Slice 3 test 1 asserts
// both as string equality rather than as parsed-URL equivalence. That is not
// pedantry: a client that fetches `https://gm.agent-wx.app/` and compares the
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

type protectedResourceDoc struct {
	Resource               string   `json:"resource"`
	AuthorizationServers   []string `json:"authorization_servers"`
	ScopesSupported        []string `json:"scopes_supported"`
	BearerMethodsSupported []string `json:"bearer_methods_supported"`
	ResourceDocumentation  string   `json:"resource_documentation"`
}

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
	return protectedResourceDoc{
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
		Issuer:                            s.Issuer(),
		AuthorizationEndpoint:             s.Issuer() + "/oauth/authorize",
		TokenEndpoint:                     s.Issuer() + "/oauth/token",
		RegistrationEndpoint:              s.Issuer() + "/oauth/register",
		RevocationEndpoint:                s.Issuer() + "/oauth/revoke",
		ResponseTypesSupported:            []string{"code"},
		GrantTypesSupported:               []string{"authorization_code", "refresh_token"},
		TokenEndpointAuthMethodsSupported: []string{"none"},
		CodeChallengeMethodsSupported:     []string{"S256"},
		ScopesSupported:                   ScopesSupported,
		AuthorizationResponseISSParameterSupported: true,
	}
}

func (s *Server) protectedResource(w http.ResponseWriter, _ *http.Request) {
	s.writeJSON(w, http.StatusOK, s.ProtectedResourceDocument())
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
