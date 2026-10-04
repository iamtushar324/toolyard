//go:build !linux && !darwin

package previewassertion

// Unsupported platforms keep the preview feature closed without affecting
// ordinary Toolyard startup or silently weakening private file guarantees.
func PrivateFile(string, uint32, int64) ([]byte, error) { return nil, ErrIdentity }
