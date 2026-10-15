package keyguard

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/tusharbhardwaj/toolyard/internal/sealbox"
)

// GeneratedKey describes a key file written by WriteNewKeyFile or
// WriteNewKeyFiles: never the key itself.
type GeneratedKey struct {
	Name  string // key file name, e.g. identity.key
	Path  string
	KeyID string // KeyID(Name, key): the fingerprint keys-check and the logs show
}

// SpecByName returns the ServeKeys spec named name: the file name
// ("identity.key") or the short name ("identity").
func SpecByName(name string) (Spec, bool) {
	n := strings.ToLower(strings.TrimSpace(name))
	if !strings.HasSuffix(n, ".key") {
		n += ".key"
	}
	for _, sp := range ServeKeys() {
		if sp.Name == n {
			return sp, true
		}
	}
	return Spec{}, false
}

// GenerateKey returns a new random master key and its file form: the
// unpadded base64 of sealbox.MasterKeySize bytes from crypto/rand, which is
// what sealbox.CreateKeyFile writes when serve creates a key and what
// LoadAll and ReadKeys decode. It is for output that is not a new file (a
// key written to stdout and piped into a secret store); WriteNewKeyFile
// writes files.
func GenerateKey() (key, encoded []byte, err error) {
	key = make([]byte, sealbox.MasterKeySize)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, nil, fmt.Errorf("keyguard: read random bytes: %w", err)
	}
	encoded = sealbox.EncodeKey(key)
	// The loader must read back exactly this key.
	if back, err := sealbox.DecodeKey(encoded); err != nil || !bytes.Equal(back, key) {
		return nil, nil, errors.New("keyguard: a generated key does not decode")
	}
	return key, encoded, nil
}

// createKeyFile is sealbox.CreateKeyFile; tests replace it to make a write
// fail after the file was created.
var createKeyFile = sealbox.CreateKeyFile

// WriteNewKeyFile writes a new random key named name (a ServeKeys file name)
// to path with sealbox.CreateKeyFile, the function serve uses to create a
// key: mode 0600, fsynced, and never over an existing file (that error
// wraps fs.ErrExist). When the write fails after the file was created (a
// write, fsync or directory sync error), the half-written file is removed,
// so a retry starts clean. A file this call did not create is never
// removed: an existing path is refused before anything else, and a failed
// open of path itself removes nothing.
func WriteNewKeyFile(path, name string) (GeneratedKey, error) {
	if _, err := os.Lstat(path); err == nil {
		return GeneratedKey{}, fmt.Errorf("%s already exists; a key file is never overwritten: %w", path, fs.ErrExist)
	}
	key, err := createKeyFile(path)
	if errors.Is(err, fs.ErrExist) {
		// Someone else's file: never touch it.
		return GeneratedKey{}, fmt.Errorf("%s already exists; a key file is never overwritten: %w", path, err)
	}
	if err != nil {
		if !openFailed(err, path) {
			// The O_EXCL open made the file (or nothing at all, when
			// reading random bytes failed: then Remove finds nothing).
			_ = os.Remove(path)
		}
		return GeneratedKey{}, fmt.Errorf("write %s: %w", path, err)
	}
	return GeneratedKey{Name: name, Path: path, KeyID: KeyID(name, key)}, nil
}

// openFailed reports that err is the failure to open path itself (EMFILE,
// EACCES and the like): the create then made no file, and whatever is at
// path belongs to someone else.
func openFailed(err error, path string) bool {
	var pe *fs.PathError
	return errors.As(err, &pe) && pe.Op == "open" && pe.Path == path
}

// WriteNewKeyFiles writes a new random key file for each spec into dir, as
// <spec.Name> (WriteNewKeyFile). dir must exist. It writes nothing when any
// of the files already exists, and removes the files it wrote when a later
// one fails, so a retry starts clean.
func WriteNewKeyFiles(dir string, specs []Spec) ([]GeneratedKey, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", dir)
	}
	var exists []string
	for _, sp := range specs {
		p := filepath.Join(dir, sp.Name)
		switch _, err := os.Lstat(p); {
		case err == nil:
			exists = append(exists, p)
		case !errors.Is(err, fs.ErrNotExist):
			return nil, err
		}
	}
	if len(exists) > 0 {
		return nil, fmt.Errorf("%s already exists; a key file is never overwritten, so nothing was written", strings.Join(exists, ", "))
	}
	out := make([]GeneratedKey, 0, len(specs))
	for _, sp := range specs {
		g, err := WriteNewKeyFile(filepath.Join(dir, sp.Name), sp.Name)
		if err != nil {
			for _, done := range out {
				_ = os.Remove(done.Path)
			}
			return nil, err
		}
		out = append(out, g)
	}
	return out, nil
}
