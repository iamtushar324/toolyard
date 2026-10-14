package keyguard

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/joho/godotenv"
)

// EnvFileName is the optional environment file read from the credentials
// directories (see LoadEnvFiles).
const EnvFileName = "toolyard.env"

// EnvOrFile returns the environment variable key, or else the contents of
// the file named by key+"_FILE" (the Docker and systemd secrets
// convention), with surrounding whitespace trimmed. Neither set returns "".
// Both set, an unreadable file or an empty file is an error matching
// ErrConfig; the error names the variable and the path, never the value.
func EnvOrFile(key string) (string, error) {
	val := strings.TrimSpace(os.Getenv(key))
	path := strings.TrimSpace(os.Getenv(key + "_FILE"))
	if path == "" {
		return val, nil
	}
	if val != "" {
		return "", configErrorf("set %s or %s_FILE, not both", key, key)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", configErrorf("%s_FILE: cannot read %s: %v", key, path, err)
	}
	v := strings.TrimSpace(string(b))
	if v == "" {
		return "", configErrorf("%s_FILE: %s is empty", key, path)
	}
	return v, nil
}

// LoadEnvFiles loads toolyard.env from $CREDENTIALS_DIRECTORY and then from
// credentialsDir, when present, and returns the files it loaded. A
// variable already set wins over both files, and the first file wins over
// the second (godotenv.Load never overrides). A file that exists but
// cannot be read or parsed is an error matching ErrConfig.
func LoadEnvFiles(credentialsDir string) ([]string, error) {
	var loaded []string
	seen := map[string]bool{}
	for _, d := range []string{os.Getenv("CREDENTIALS_DIRECTORY"), credentialsDir} {
		if d = strings.TrimSpace(d); d == "" {
			continue
		}
		path := filepath.Join(filepath.Clean(d), EnvFileName)
		if seen[path] {
			continue
		}
		seen[path] = true
		if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
			continue
		} else if err != nil {
			return loaded, configErrorf("%s: %v", path, err)
		}
		if err := godotenv.Load(path); err != nil {
			// The parser's message can quote the file's contents: leave
			// it out, since the file holds secrets.
			return loaded, configErrorf("%s cannot be read as an environment file (KEY=value lines)", path)
		}
		loaded = append(loaded, path)
	}
	return loaded, nil
}
