package codemode

// handleCall makes one call through the session the way a worker's call
// message does, and waits for its answer.
func (s *session) handleCall(server, member string, args map[string]any) parentMsg {
	_, ch := s.start(callRequest{server: server, member: member, args: args})
	return <-ch
}
