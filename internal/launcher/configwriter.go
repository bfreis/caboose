package launcher

import (
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/bfreis/caboose/internal/broker"
	"github.com/bfreis/caboose/internal/config"
)

// configWriter is the broker's ConfigWriter: it makes an "always" answer's
// edit to the environment's config.toml the way setup does, line by line
// through config.EditFile and checked with config.CheckEdit -- never a
// marshalled file, so comments and unrelated lines stay as they are.
type configWriter struct {
	envDir string
	mu     sync.Mutex // one write at a time in this process
}

var _ broker.ConfigWriter = (*configWriter)(nil)

// plan reads config.toml and makes e on it, checked: the file as it is,
// as it would be, and what it would read back as.
func (w *configWriter) plan(e config.Edit) (path string, data, edited []byte, mode os.FileMode, err error) {
	if w.envDir == "" {
		return "", nil, nil, 0, errors.New("this environment has no dir of its own, so no config.toml")
	}
	path, data, m, err := readConfigToEdit(w.envDir)
	if err != nil {
		return "", nil, nil, 0, err
	}
	edited = config.EditFile(data, e)
	if string(edited) != string(data) {
		if err := config.CheckEdit(path, data, edited, e); err != nil {
			return "", nil, nil, 0, fmt.Errorf("cannot edit %s safely (%v); nothing was written to it", path, err)
		}
	}
	return path, data, edited, m, nil
}

// Diff is the change e would make to config.toml; nothing is written.
func (w *configWriter) Diff(e config.Edit) (string, error) {
	_, data, edited, _, err := w.plan(e)
	if err != nil {
		return "", err
	}
	return lineDiff(string(data), string(edited)), nil
}

// Write makes e, provided it still changes config.toml as the diff that
// hashes to diffHash showed: the file is read again, the edit made and
// checked again, and anything else is broker.ErrStaleDiff, writing
// nothing.
func (w *configWriter) Write(e config.Edit, diffHash string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	path, data, edited, mode, err := w.plan(e)
	if err != nil {
		return err
	}
	if broker.DiffHash(lineDiff(string(data), string(edited))) != diffHash {
		return broker.ErrStaleDiff
	}
	if string(edited) == string(data) {
		return nil
	}
	return replaceConfig(w.envDir, path, edited, mode)
}
