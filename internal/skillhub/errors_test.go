package skillhub

import (
	"errors"
	"testing"
)

// A missing GitHub connection is both ErrInvalid (as before) and
// ErrNoGitHubConnection, with a message free of sentinel words.
func TestNeedsConnection(t *testing.T) {
	err := needsConnection("the owner has no usable toolyard GitHub connection")
	if !errors.Is(err, ErrInvalid) || !errors.Is(err, ErrNoGitHubConnection) || errors.Is(err, ErrConflict) {
		t.Fatalf("errors.Is: %v", err)
	}
	if err.Error() != "skillhub: the owner has no usable toolyard GitHub connection" {
		t.Fatalf("message %q", err.Error())
	}
}
