// Package cappedlog is a log file appended to across runs, so a restart
// keeps what the last run said, and moved aside rather than grow past a cap:
// whatever writes to it may be told what to log by something it does not
// control (a sandbox), so it must not fill the disk.
package cappedlog

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Log is an open capped log. Whenever a write would take it past its cap it
// is moved to path.1 (replacing the one before) and started again, with a
// line saying so.
type Log struct {
	mu   sync.Mutex
	path string
	who  string
	f    *os.File
	n    int64
	max  int64
}

// Open opens (creating, mode 0600) the log at path, capped at max bytes.
// who names the writer in the line a rotation writes ("caboose link").
func Open(path string, max int64, who string) (*Log, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	return &Log{path: path, who: who, f: f, n: fi.Size(), max: max}, nil
}

func (c *Log) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.n > 0 && c.n+int64(len(p)) > c.max {
		if err := c.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := c.f.Write(p)
	c.n += int64(n)
	return n, err
}

// rotate moves the log to path.1 and starts a new one. Under c.mu.
func (c *Log) rotate() error {
	if err := os.Rename(c.path, c.path+".1"); err != nil {
		return err
	}
	f, err := os.OpenFile(c.path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	c.f.Close()
	c.f, c.n = f, 0
	name := filepath.Base(c.path)
	m, _ := fmt.Fprintf(c.f, "%s %s: %s reached %d MiB: the lines before are in %s.1\n", time.Now().Format("2006/01/02 15:04:05"), c.who, name, c.max>>20, name)
	c.n += int64(m)
	return nil
}

// Close closes the log.
func (c *Log) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.f.Close()
}
