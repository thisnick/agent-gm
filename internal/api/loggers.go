package api

// SetLoggers replaces the server's two log hooks.
//
// There are two, and the split is the point (see Deps): `refused` is ordinary
// traffic an operator reads when they go looking, and `internalError` is a
// bug they must be told about without having to. Wiring the second to the
// first, at debug level, is what made a 500 produce no log line at all for a
// whole slice while its own message told the caller to look in the logs.
func (s *Server) SetLoggers(refused, internalError func(msg string, kv ...any)) {
	s.deps.Log = refused
	s.deps.LogError = internalError
}
