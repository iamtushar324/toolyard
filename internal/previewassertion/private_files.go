//go:build linux || darwin

package previewassertion

import (
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// PrivateFile traverses from an opened root directory using only no-follow
// descriptors. No path-based stat/open race can substitute an ancestor or leaf.
// The authenticated service and privileged host operators remain trusted.
func PrivateFile(path string, ownerUID uint32, maximum int64) ([]byte, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || maximum < 1 {
		return nil, ErrIdentity
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(parts) < 2 {
		return nil, ErrIdentity
	}
	dir, err := unix.Open("/", unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, ErrIdentity
	}
	defer func() { unix.Close(dir) }()
	for i := 0; ; i++ {
		var st unix.Stat_t
		if unix.Fstat(dir, &st) != nil || st.Mode&unix.S_IFMT != unix.S_IFDIR || (st.Uid != 0 && st.Uid != ownerUID) || st.Mode&0022 != 0 {
			return nil, ErrIdentity
		}
		if i == len(parts)-1 {
			if st.Uid != ownerUID || st.Mode&0077 != 0 {
				return nil, ErrIdentity
			}
			break
		}
		next, err := unix.Openat(dir, parts[i], unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
		if err != nil {
			return nil, ErrIdentity
		}
		unix.Close(dir)
		dir = next
	}
	fd, err := unix.Openat(dir, parts[len(parts)-1], unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, ErrIdentity
	}
	f := os.NewFile(uintptr(fd), "private-preview-file")
	if f == nil {
		unix.Close(fd)
		return nil, ErrIdentity
	}
	defer f.Close()
	var st unix.Stat_t
	if unix.Fstat(fd, &st) != nil || st.Mode&unix.S_IFMT != unix.S_IFREG || st.Uid != ownerUID || st.Mode&0077 != 0 || st.Mode&0111 != 0 || st.Nlink != 1 || st.Size > maximum {
		return nil, ErrIdentity
	}
	raw, err := io.ReadAll(io.LimitReader(f, maximum+1))
	if err != nil || int64(len(raw)) > maximum {
		return nil, ErrIdentity
	}
	return raw, nil
}
