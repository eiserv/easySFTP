package autocache

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// fileMode is what the cache file is created with. It holds no secrets (the
// target is a fingerprint, see Record.Target), but it describes somebody's
// deployment and there is no reason for it to be world readable.
const fileMode = 0o600

// ErrEmpty reports a path that holds no usable store yet: the file does not
// exist, or it is empty. It is the normal state of a first run and the caller
// says so at debug level rather than warning about it.
var ErrEmpty = errors.New("no auto-tuning cache yet")

// Load reads the store at path.
//
// Every failure mode here is recoverable by design, because the cache is an
// optimisation and must never be able to fail a deploy (issue #212). A missing
// file is ErrEmpty. A file that cannot be parsed, or that was written by a
// newer easySFTP, comes back as an error together with an empty usable store,
// so the caller can warn once and carry on with the built-in prior. The one
// thing Load will not do is guess: a version it does not know is not read
// field by field in the hope that the fields still mean what they meant.
func Load(path string) (*Store, error) {
	if path == "" {
		return &Store{Version: FormatVersion}, ErrEmpty
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &Store{Version: FormatVersion}, ErrEmpty
		}
		return &Store{Version: FormatVersion}, err
	}
	if len(data) == 0 {
		return &Store{Version: FormatVersion}, ErrEmpty
	}
	var s Store
	if err := json.Unmarshal(data, &s); err != nil {
		return &Store{Version: FormatVersion}, fmt.Errorf("parsing %s: %w", path, err)
	}
	if s.Version != FormatVersion {
		return &Store{Version: FormatVersion}, fmt.Errorf(
			"%s is version %d and this easySFTP writes version %d", path, s.Version, FormatVersion)
	}
	s.Version = FormatVersion
	return &s, nil
}

// Save writes the store to path, creating the directory if it is missing (a
// cache path pointed at a directory a CI cache has not restored yet is the
// normal first run, not an error).
//
// The write is atomic: a temporary sibling, then a rename. A cache file
// truncated by a cancelled job would be read back as corrupt on the next run,
// which costs a warning and a re-measure; it is cheap to make that impossible.
func Save(path string, s *Store) error {
	if path == "" || s == nil {
		return nil
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	s.Version = FormatVersion
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name) // no-op once the rename succeeded
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(name, fileMode); err != nil {
		return err
	}
	return os.Rename(name, path)
}
