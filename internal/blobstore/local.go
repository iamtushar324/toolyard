package blobstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// local keeps each blob as the file dir/<sha256>, mode 0600 (os.CreateTemp
// then a rename). This is the layout toolyard has always used.
type local struct {
	dir string
}

// NewLocal stores blobs as files in dir, creating it (mode 0700) if missing.
func NewLocal(dir string) (Store, error) {
	if dir == "" {
		return nil, errors.New("blobstore: no directory for local blobs")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &local{dir: filepath.Clean(dir)}, nil
}

func (l *local) path(sha string) string { return filepath.Join(l.dir, sha) }

// Put writes r to a temporary file in the directory and renames it into
// place. Nothing is written when the blob is already there.
func (l *local) Put(ctx context.Context, sha string, r io.Reader, size int64) (bool, error) {
	if !ValidSHA(sha) {
		return false, errBadName(sha)
	}
	if size < 0 {
		return false, fmt.Errorf("blobstore: negative size %d", size)
	}
	if _, err := os.Stat(l.path(sha)); err == nil {
		return false, nil
	}
	tmp, err := os.CreateTemp(l.dir, "put-*")
	if err != nil {
		return false, err
	}
	tmpPath := tmp.Name()
	n, err := io.Copy(tmp, io.LimitReader(r, size+1))
	if cerr := tmp.Close(); err == nil && cerr != nil {
		err = cerr
	}
	if err == nil && n != size {
		err = fmt.Errorf("blobstore: %s has %d bytes, want %d", short(sha), n, size)
	}
	if err != nil {
		_ = os.Remove(tmpPath)
		return false, err
	}
	return l.AdoptFile(ctx, sha, tmpPath)
}

// AdoptFile renames path (a file in the store's directory) to the blob's
// name. When the blob already exists, path is removed instead.
func (l *local) AdoptFile(_ context.Context, sha, path string) (bool, error) {
	if !ValidSHA(sha) || filepath.Dir(path) != l.dir {
		_ = os.Remove(path)
		return false, fmt.Errorf("blobstore: can't adopt %s as blob %q", filepath.Base(path), short(sha))
	}
	dst := l.path(sha)
	if _, err := os.Stat(dst); err == nil {
		_ = os.Remove(path)
		return false, nil
	}
	if err := os.Rename(path, dst); err != nil {
		_ = os.Remove(path)
		return false, err
	}
	return true, nil
}

// Open opens the blob's file.
func (l *local) Open(_ context.Context, sha string) (Blob, error) {
	if !ValidSHA(sha) {
		return nil, ErrNotFound
	}
	f, err := os.Open(l.path(sha))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return &localBlob{File: f, size: st.Size()}, nil
}

// Exists reports whether the blob's file is there.
func (l *local) Exists(_ context.Context, sha string) (bool, error) {
	if !ValidSHA(sha) {
		return false, nil
	}
	_, err := os.Stat(l.path(sha))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}

// Delete removes the blob's file; a missing file is not an error.
func (l *local) Delete(_ context.Context, sha string) error {
	if !ValidSHA(sha) {
		return nil
	}
	if err := os.Remove(l.path(sha)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// localBlob is an open blob file. It keeps *os.File's methods, Stat
// included, so callers can still read the file's modification time.
type localBlob struct {
	*os.File
	size int64
}

func (b *localBlob) Size() int64 { return b.size }
