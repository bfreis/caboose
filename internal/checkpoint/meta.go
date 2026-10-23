package checkpoint

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

// MetaVersion is the meta.json and path.json version this caboose writes.
const MetaVersion = 1

// maxMetaSize bounds what is read of a meta.json or a path.json.
const maxMetaSize = 64 << 10

// ErrNewerVersion is returned for a meta.json written by a newer caboose:
// such a checkpoint is listed as unreadable and never pruned.
var ErrNewerVersion = errors.New("checkpoint: written by a newer caboose")

// How a checkpoint was copied.
const (
	MethodClonefile = "clonefile"
	MethodReflink   = "reflink"
	MethodCopy      = "copy"
)

// Why a checkpoint was taken.
const (
	ReasonInterval = "interval"
	ReasonStart    = "start"
	ReasonStop     = "stop"
	ReasonManual   = "manual"
	// ReasonAgent is one the sandbox asked for (caboose-agent checkpoint).
	ReasonAgent         = "agent"
	ReasonBeforeRestore = "before-restore"
)

// What a checkpoint's tree is.
const (
	KindDir  = "dir"
	KindFile = "file"
)

// Meta is a checkpoint's meta.json, written last, so a checkpoint without
// one is not a checkpoint.
type Meta struct {
	Version  int       `json:"version"`
	ID       string    `json:"id"`
	Path     string    `json:"path"`   // the configured path
	Kind     string    `json:"kind"`   // KindDir or KindFile
	Source   string    `json:"source"` // the physical host path at the time
	Method   string    `json:"method"`
	Reason   string    `json:"reason"`
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished"`
	Entries  int64     `json:"entries"`
	Bytes    int64     `json:"bytes"` // regular files' logical size
	// Shrunk is set when this checkpoint holds far less than the one
	// before it, which it names.
	Shrunk          *Shrunk  `json:"shrunk,omitempty"`
	Linked          int64    `json:"linked"` // files hard-linked from the previous checkpoint
	Skipped         int64    `json:"skipped"`
	SkippedExamples []string `json:"skipped_examples,omitempty"` // a few relative paths
	Dropped         []string `json:"dropped,omitempty"`          // login files left out
	Caboose         string   `json:"caboose"`                    // the version that took it
}

// Shrunk is the previous checkpoint's counts, when this one fell sharply.
type Shrunk struct {
	ID      string `json:"id"`
	Entries int64  `json:"entries"`
	Bytes   int64  `json:"bytes"`
}

// readBounded reads all of r, refusing more than maxMetaSize bytes.
func readBounded(r io.Reader) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, maxMetaSize+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxMetaSize {
		return nil, fmt.Errorf("checkpoint: over %d KiB", maxMetaSize>>10)
	}
	return b, nil
}

// checkVersion reads a document's version: ErrNewerVersion above ours, an
// error below 1.
func checkVersion(b []byte) error {
	var v struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return fmt.Errorf("checkpoint: %w", err)
	}
	if v.Version > MetaVersion {
		return fmt.Errorf("%w (version %d)", ErrNewerVersion, v.Version)
	}
	if v.Version < 1 {
		return fmt.Errorf("checkpoint: bad version %d", v.Version)
	}
	return nil
}

// DecodeMeta reads a meta.json, at most 64 KiB of it.
func DecodeMeta(r io.Reader) (*Meta, error) {
	b, err := readBounded(r)
	if err != nil {
		return nil, err
	}
	if err := checkVersion(b); err != nil {
		return nil, err
	}
	var m Meta
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("checkpoint: %w", err)
	}
	return &m, nil
}

// pathDoc is a key's path.json.
type pathDoc struct {
	Version int    `json:"version"`
	Path    string `json:"path"`
}

func decodePath(r io.Reader) (string, error) {
	b, err := readBounded(r)
	if err != nil {
		return "", err
	}
	if err := checkVersion(b); err != nil {
		return "", err
	}
	var d pathDoc
	if err := json.Unmarshal(b, &d); err != nil {
		return "", fmt.Errorf("checkpoint: %w", err)
	}
	if d.Path == "" {
		return "", errors.New("checkpoint: path.json names no path")
	}
	return d.Path, nil
}
