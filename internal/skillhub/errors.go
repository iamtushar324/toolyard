package skillhub

// connectionError is a refusal because a GitHub connection is missing: it
// is both ErrInvalid (the request cannot be served as it is) and
// ErrNoGitHubConnection, so the agent surface can answer "not available"
// with this message instead of "invalid input".
type connectionError struct{ msg string }

func (e *connectionError) Error() string { return "skillhub: " + e.msg }

// Is matches ErrInvalid and ErrNoGitHubConnection.
func (e *connectionError) Is(target error) bool {
	return target == ErrInvalid || target == ErrNoGitHubConnection
}

// needsConnection is a connectionError with msg.
func needsConnection(msg string) error { return &connectionError{msg: msg} }
