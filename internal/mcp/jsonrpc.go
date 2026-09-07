package mcp

import "encoding/json"

// JSON-RPC 2.0, as much of it as the MCP streamable HTTP transport uses.

// The two protocol revisions this server speaks (spec section 8.1).
const (
	// ProtocolVersion is what `initialize` answers with when the client asks
	// for it or asks for nothing.
	ProtocolVersion = "2026-07-28"
	// ProtocolVersionCompat is accepted for compatibility.
	ProtocolVersionCompat = "2025-11-25"
)

// SupportedProtocolVersions is the set, newest first.
var SupportedProtocolVersions = []string{ProtocolVersion, ProtocolVersionCompat}

func supportedProtocol(v string) bool {
	for _, s := range SupportedProtocolVersions {
		if s == v {
			return true
		}
	}
	return false
}

// jsonrpcRequest is one call. `id` is absent on a notification, which is
// answered with no body at all.
type jsonrpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

func (r jsonrpcRequest) isNotification() bool { return len(r.ID) == 0 }

// jsonrpcError is the error object.
type jsonrpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// jsonrpcResponse is one answer.
type jsonrpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *jsonrpcError   `json:"error,omitempty"`
}

// The JSON-RPC codes this server uses.
//
// Spec section 8.2 fixes what may be reported this way at all: **only a
// malformed request, an unknown method, an unknown tool name, or an
// authorization failure at the transport.** A domain failure -- `not_found`,
// `invalid_request`, `unsupported_capability` and every other code of section
// 7.2 -- is a RESULT with `isError: true`, because many MCP clients surface a
// JSON-RPC error as a transport failure and never hand it to the model, and a
// `not_found` reported that way is a fact the model never learns and cannot
// correct itself from.
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternalError  = -32603
)

func rpcResult(id json.RawMessage, result any) jsonrpcResponse {
	return jsonrpcResponse{JSONRPC: "2.0", ID: id, Result: result}
}

func rpcFail(id json.RawMessage, code int, message string) jsonrpcResponse {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	return jsonrpcResponse{JSONRPC: "2.0", ID: id, Error: &jsonrpcError{Code: code, Message: message}}
}
