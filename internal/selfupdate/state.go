package selfupdate

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// StateFile and LockFile live in CABOOSE_HOME itself, not in an
// environment: one binary serves them all.
const (
	StateFile = "update.json"
	LockFile  = "update.lock"
)

// State is what the updater remembers between runs.
type State struct {
	// Attempted is when a launch last started a check: at most one a day.
	Attempted time.Time `json:"attempted,omitzero"`
	// Checked is when a check last got an answer, or failed trying; Latest
	// is what it found, Error why it failed ("" when it did not).
	Checked time.Time `json:"checked,omitzero"`
	Latest  string    `json:"latest,omitempty"`
	Error   string    `json:"error,omitempty"`
	// From and To are the last update made; Announced, whether a run of To
	// has said so yet.
	From      string `json:"from,omitempty"`
	To        string `json:"to,omitempty"`
	Announced bool   `json:"announced,omitempty"`
}

// ReadState reads home's state; none, or one that does not parse, is the
// zero State.
func ReadState(home string) State {
	var st State
	b, err := os.ReadFile(filepath.Join(home, StateFile))
	if err == nil {
		_ = json.Unmarshal(b, &st)
	}
	return st
}

// WriteState replaces home's state, by rename, so a reader never sees half
// of it. home is made when it does not exist.
func WriteState(home string, st State) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(home, "."+StateFile+"-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = f.Write(append(b, '\n'))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), filepath.Join(home, StateFile))
}

// ErrLocked is an update already running.
var ErrLocked = errors.New("an update is already running")

// Lock takes home's update lock without waiting: two launches a moment
// apart start one download, not two. The lock goes with the process, so
// one that dies leaves none behind.
func Lock(home string) (unlock func(), err error) {
	if err := os.MkdirAll(home, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(home, LockFile), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrLocked
		}
		return nil, &fs.PathError{Op: "flock", Path: f.Name(), Err: err}
	}
	return func() { f.Close() }, nil
}
