package skillhub

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"unicode/utf8"
)

// errUnsafeArchive is an archive entry whose path would leave the
// archive (absolute, or with a ".." segment): the whole download is
// refused.
var errUnsafeArchive = errors.New("skillhub: the repository archive holds an unsafe path")

// candidate is one text file a version may serve.
type candidate struct {
	path  string // relative to the source folder
	sha   string // "sha256:<hex>"
	size  int64
	media string
	data  []byte // nil when the blob is stored already (incremental update)
}

// fileSet is every text file under the source folder that passed the
// file checks, and what was left out. Paths are relative to the source
// folder.
type fileSet struct {
	files    map[string]*candidate
	omitted  map[string]Warning // file checks that failed, by path
	excluded []string           // excluded files (capped)
	bytes    int64
	lim      effLimits
}

// effLimits are Limits with the defaults filled in.
type effLimits struct {
	fileBytes  int64
	totalBytes int64
	skills     int
	files      int
}

func (l Limits) effective() effLimits {
	e := effLimits{fileBytes: l.MaxFileBytes, totalBytes: l.MaxTotalBytes, skills: l.MaxSkills, files: l.MaxFiles}
	if e.fileBytes <= 0 {
		e.fileBytes = DefaultMaxFileBytes
	}
	if e.totalBytes <= 0 {
		e.totalBytes = DefaultMaxTotalBytes
	}
	if e.skills <= 0 {
		e.skills = DefaultMaxSkills
	}
	if e.files <= 0 {
		e.files = DefaultMaxFiles
	}
	return e
}

// maxExcludedKept caps the excluded paths a fileSet remembers (for the
// "excluded" warnings of files inside skill folders).
const maxExcludedKept = 10000

func newFileSet(lim effLimits) *fileSet {
	return &fileSet{files: map[string]*candidate{}, omitted: map[string]Warning{}, lim: lim}
}

// omit records a file the checks left out.
func (fs *fileSet) omit(rel, code, msg string) {
	fs.omitted[rel] = Warning{Path: rel, Code: code, Message: msg, Severity: "skip"}
}

// exclude records a file an exclude glob left out.
func (fs *fileSet) exclude(rel string) {
	if len(fs.excluded) < maxExcludedKept {
		fs.excluded = append(fs.excluded, rel)
	}
}

// remove forgets a file (an incremental update deleted or replaced it).
func (fs *fileSet) remove(rel string) {
	if c, ok := fs.files[rel]; ok {
		fs.bytes -= c.size
		delete(fs.files, rel)
	}
	delete(fs.omitted, rel)
}

// add checks one file's content and keeps it, or records why not. Over
// the source's file or byte limit it fails.
func (fs *fileSet) add(rel string, data []byte) error {
	if int64(len(data)) > fs.lim.fileBytes {
		fs.omit(rel, "too_large", fmt.Sprintf("omitted: %d bytes, over the %d-byte file limit", len(data), fs.lim.fileBytes))
		return nil
	}
	if !isText(data) {
		fs.omit(rel, "binary", "omitted: not a text file (binary or not UTF-8)")
		return nil
	}
	if what := findSecret(data); what != "" {
		fs.omit(rel, "secret", "omitted: it looks like it holds "+what)
		return nil
	}
	if len(fs.files) >= fs.lim.files {
		return fmt.Errorf("%w: more than %d files under the source folder (narrow the folder or add exclude globs)", ErrLimit, fs.lim.files)
	}
	if fs.bytes+int64(len(data)) > fs.lim.totalBytes {
		return fmt.Errorf("%w: more than %d bytes of text under the source folder (narrow the folder or add exclude globs)", ErrLimit, fs.lim.totalBytes)
	}
	fs.files[rel] = &candidate{path: rel, sha: digest(data), size: int64(len(data)), media: mediaType(rel), data: data}
	fs.bytes += int64(len(data))
	return nil
}

// isText reports UTF-8 text with no NUL byte.
func isText(data []byte) bool {
	return bytes.IndexByte(data, 0) < 0 && utf8.Valid(data)
}

// readArchive reads a GitHub tarball (gzip; one top folder) and returns
// the text files under folder. It streams: only kept files are held, and
// those within the source's limits.
func readArchive(r io.Reader, folder string, ex *excluder, lim effLimits) (*fileSet, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("skillhub: read archive: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(&capReader{r: gz, left: maxArchiveBytes})
	fs := newFileSet(lim)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return fs, nil
		}
		if errors.Is(err, tar.ErrInsecurePath) && hdr != nil {
			return nil, fmt.Errorf("%w: %q", errUnsafeArchive, hdr.Name)
		}
		if err != nil {
			return nil, fmt.Errorf("skillhub: read archive: %w", err)
		}
		if hdr.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		name, err := archivePath(hdr.Name)
		if err != nil {
			return nil, err
		}
		rel, ok := underFolder(name, folder)
		if !ok || rel == "" {
			continue
		}
		if err := fs.addEntry(tr, hdr, rel, ex); err != nil {
			return nil, err
		}
	}
}

// addEntry handles one archive entry under the source folder.
func (fs *fileSet) addEntry(tr *tar.Reader, hdr *tar.Header, rel string, ex *excluder) error {
	switch hdr.Typeflag {
	case tar.TypeReg, '\x00':
	case tar.TypeDir:
		return nil
	case tar.TypeSymlink, tar.TypeLink:
		if !ex.excluded(rel) {
			fs.omit(rel, "symlink", "omitted: links are not followed")
		}
		return nil
	case tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
		if !ex.excluded(rel) {
			fs.omit(rel, "special", "omitted: not a regular file")
		}
		return nil
	default:
		return nil
	}
	if ex.excluded(rel) {
		fs.exclude(rel)
		return nil
	}
	if hdr.Size > fs.lim.fileBytes {
		fs.omit(rel, "too_large", fmt.Sprintf("omitted: %d bytes, over the %d-byte file limit", hdr.Size, fs.lim.fileBytes))
		return nil
	}
	data, err := io.ReadAll(io.LimitReader(tr, hdr.Size))
	if err != nil {
		return fmt.Errorf("skillhub: read archive: %w", err)
	}
	return fs.add(rel, data)
}

// archivePath checks an archive entry name and strips its top folder.
// Absolute names and ".." segments are refused.
func archivePath(name string) (string, error) {
	if strings.HasPrefix(name, "/") || strings.Contains(name, "\\") || strings.ContainsRune(name, 0) {
		return "", fmt.Errorf("%w: %q", errUnsafeArchive, name)
	}
	segs := strings.Split(strings.TrimSuffix(name, "/"), "/")
	for _, seg := range segs {
		if seg == ".." {
			return "", fmt.Errorf("%w: %q", errUnsafeArchive, name)
		}
	}
	var kept []string
	for _, seg := range segs[1:] {
		if seg == "" || seg == "." {
			continue
		}
		kept = append(kept, seg)
	}
	return strings.Join(kept, "/"), nil
}

// underFolder returns name relative to folder, and whether it is inside.
func underFolder(name, folder string) (string, bool) {
	if folder == "" {
		return name, true
	}
	if name == folder {
		return "", true
	}
	if strings.HasPrefix(name, folder+"/") {
		return name[len(folder)+1:], true
	}
	return "", false
}

// capReader fails once more than left bytes were read (a decompression
// bomb).
type capReader struct {
	r    io.Reader
	left int64
}

func (c *capReader) Read(p []byte) (int, error) {
	if c.left <= 0 {
		return 0, fmt.Errorf("%w: the repository archive is larger than %d bytes uncompressed", ErrLimit, int64(maxArchiveBytes))
	}
	if int64(len(p)) > c.left {
		p = p[:c.left]
	}
	n, err := c.r.Read(p)
	c.left -= int64(n)
	return n, err
}

// mediaTypes maps file extensions to the media type a file is served
// with. Every served file is UTF-8 text; unknown extensions are
// text/plain.
var mediaTypes = map[string]string{
	".md":       "text/markdown",
	".markdown": "text/markdown",
	".txt":      "text/plain",
	".py":       "text/x-python",
	".sh":       "text/x-shellscript",
	".bash":     "text/x-shellscript",
	".js":       "text/javascript",
	".mjs":      "text/javascript",
	".cjs":      "text/javascript",
	".ts":       "text/x-typescript",
	".json":     "application/json",
	".yaml":     "application/yaml",
	".yml":      "application/yaml",
	".toml":     "application/toml",
	".xml":      "application/xml",
	".html":     "text/html",
	".htm":      "text/html",
	".css":      "text/css",
	".csv":      "text/csv",
	".svg":      "image/svg+xml",
	".sql":      "application/sql",
	".go":       "text/x-go",
	".rb":       "text/x-ruby",
}

func mediaType(p string) string {
	if t, ok := mediaTypes[strings.ToLower(path.Ext(p))]; ok {
		return t
	}
	return "text/plain"
}

// sortedPaths returns the keys of m, sorted.
func sortedPaths[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
