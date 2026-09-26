package datadir

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/bfreis/caboose/internal/nofollow"
	"github.com/bfreis/caboose/internal/sandboxcfg"
)

// Sandbox is the sandbox config in effect for a data dir, as
// LoadSandboxConfig found it.
type Sandbox struct {
	// Config is what is in effect.
	*sandboxcfg.Config
	// Data is the text it was read from: the file, the last good copy, or
	// the defaults.
	Data []byte
	// Absent is set when there is no file: the defaults are in effect,
	// and nothing has written them yet.
	Absent bool
	// Err is why the file itself could not be used (it does not parse, or
	// is of a newer format: sandboxcfg.ErrNewerFormat), when Config is the
	// last good copy, or else the defaults, instead.
	Err error
	// LastGood is set when Config is the last good copy.
	LastGood bool
}

// LoadSandboxConfig reads the data dir's sandbox config, through nofollow:
// the sandbox writes it. One that parses is also kept, as LastGoodConfig,
// in the data dir itself, which no container mounts; one that does not is
// replaced by that copy (or by the defaults, when there is none), with
// Err saying why. With no file at all, the defaults are in effect; roots
// are what they name (the host config's root names).
func LoadSandboxConfig(dir string, roots []string) (*Sandbox, error) {
	data, _, err := nofollow.Dir(dir).ReadFile(SandboxConfig)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		d := sandboxcfg.Default(roots)
		c, err := sandboxcfg.Parse(d)
		return &Sandbox{Config: c, Data: d, Absent: true}, err
	case errors.Is(err, nofollow.ErrNotPlain):
		// Read as one that does not parse: the last good copy stands in.
	case err != nil:
		return nil, err
	}
	var bad error = err
	if err == nil {
		c, perr := sandboxcfg.Parse(data)
		if perr == nil {
			last := filepath.Join(dir, LastGoodConfig)
			if have, err := os.ReadFile(last); err != nil || !bytes.Equal(have, data) {
				_ = os.WriteFile(last, data, 0o600) // best-effort: only a fallback
			}
			return &Sandbox{Config: c, Data: data}, nil
		}
		bad = perr
	}
	if last, err := os.ReadFile(filepath.Join(dir, LastGoodConfig)); err == nil {
		if c, err := sandboxcfg.Parse(last); err == nil {
			return &Sandbox{Config: c, Data: last, Err: bad, LastGood: true}, nil
		}
	}
	d := sandboxcfg.Default(roots)
	c, err := sandboxcfg.Parse(d)
	return &Sandbox{Config: c, Data: d, Err: bad}, err
}
