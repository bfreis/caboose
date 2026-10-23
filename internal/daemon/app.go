package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/bfreis/caboose/internal/daemonproto"
)

// connPeer is a socket connection's peer, read off the socket as the
// connection was taken: never anything the client sends.
type connPeer struct {
	pid int    // 0 when the system does not say
	exe string // "" when it could not be read
}

// attachment is the attached menu-bar app.
type attachment struct {
	conn    *connPeer // its connection: the attachment ends with it
	n       uint64    // its number, which Dialogs knows it by
	version string
	caps    []string
	since   time.Time
}

// knownCaps are the capabilities an attachment keeps, in this order.
var knownCaps = []string{daemonproto.CapDialogs, daemonproto.CapNotifications}

// notAttached is the refusal of an app method to a connection that is not
// the attached app's.
func notAttached() *daemonproto.Error {
	return daemonproto.NewError(daemonproto.KindBadRequest, "only the menu-bar app's attached connection may call this")
}

// runAppAttach is app_attach: see daemonproto.AppAttachParams.
func (s *server) runAppAttach(_ context.Context, c caller, params json.RawMessage) (any, func(), *daemonproto.Error) {
	if c.conn == nil || c.Kind != daemonproto.ClientApp {
		return nil, nil, notAttached()
	}
	var p daemonproto.AppAttachParams
	if e := decode(daemonproto.MethodAppAttach, params, &p); e != nil {
		return nil, nil, e
	}
	caps := []string{}
	for _, k := range knownCaps {
		if slices.Contains(p.Capabilities, k) {
			caps = append(caps, k)
		}
	}
	s.mu.Lock()
	if s.app != nil && s.app.conn != c.conn {
		pid := s.app.conn.pid
		s.mu.Unlock()
		s.log.Printf("app_attach: refused a second menu-bar app (pid %d): the app of pid %d is attached", c.conn.pid, pid)
		return nil, nil, daemonproto.NewError(daemonproto.KindBusy, "a menu-bar app is attached already (pid %d)", pid)
	}
	fresh := s.app == nil
	since := time.Now()
	var n uint64
	if fresh {
		s.attachN++
		n = s.attachN
	} else {
		since, n = s.app.since, s.app.n
	}
	s.app = &attachment{conn: c.conn, n: n, version: daemonproto.Text(p.Version, 64), caps: caps, since: since}
	// Under mu, so that a detach of the previous app cannot land after
	// this attach (the Notifier never takes mu).
	s.notes.attach(slices.Contains(caps, daemonproto.CapNotifications), fresh)
	s.mu.Unlock()
	// Not under mu (it may deny a request, which writes a file): the
	// attachment's number orders it against a late detach instead.
	if s.opts.Dialogs != nil {
		s.opts.Dialogs.SetApp(n, slices.Contains(caps, daemonproto.CapDialogs))
	}
	verb := "attached"
	if !fresh {
		verb = "changed its capabilities"
	}
	s.log.Printf("menu-bar app %s: pid %d, executable %q, caboose %s, capabilities %v", verb, c.conn.pid, c.conn.exe, daemonproto.Text(p.Version, 64), caps)
	return &daemonproto.AppAttachResult{Attached: true, Capabilities: caps}, nil, nil
}

// runNotifications is notifications: the attached app's queue.
func (s *server) runNotifications(_ context.Context, c caller, params json.RawMessage) (any, func(), *daemonproto.Error) {
	var p daemonproto.NotificationsParams
	if e := decode(daemonproto.MethodNotifications, params, &p); e != nil {
		return nil, nil, e
	}
	s.mu.Lock()
	attached := c.conn != nil && s.app != nil && s.app.conn == c.conn
	s.mu.Unlock()
	if !attached {
		return nil, nil, notAttached()
	}
	return s.notes.poll(p.After), nil, nil
}

// runDialogs is dialogs: the request whose URL dialog the attached app
// is to show (daemonproto.DialogsParams).
func (s *server) runDialogs(_ context.Context, c caller, params json.RawMessage) (any, func(), *daemonproto.Error) {
	var p daemonproto.DialogsParams
	if e := decode(daemonproto.MethodDialogs, params, &p); e != nil {
		return nil, nil, e
	}
	s.mu.Lock()
	var n uint64
	holds := false
	if c.conn != nil && s.app != nil && s.app.conn == c.conn {
		n, holds = s.app.n, slices.Contains(s.app.caps, daemonproto.CapDialogs)
	}
	s.mu.Unlock()
	if n == 0 {
		return nil, nil, notAttached()
	}
	if !holds {
		return nil, nil, daemonproto.NewError(daemonproto.KindBadRequest, "the attached app did not attach with %s", daemonproto.CapDialogs)
	}
	if s.opts.Dialogs == nil {
		return &daemonproto.DialogsResult{}, nil, nil
	}
	d, err := s.opts.Dialogs.Next(n)
	if err != nil {
		return nil, nil, protoError(err)
	}
	return &daemonproto.DialogsResult{Dialog: d}, nil, nil
}

// detachApp ends the attachment when cp is its connection's: the
// connection closed.
func (s *server) detachApp(cp *connPeer) {
	s.mu.Lock()
	ended := s.app != nil && s.app.conn == cp
	var n uint64
	if ended {
		n = s.app.n
		s.app = nil
		s.notes.detach()
	}
	s.mu.Unlock()
	if !ended {
		return
	}
	s.log.Printf("menu-bar app detached (pid %d)", cp.pid)
	if s.opts.Dialogs != nil {
		s.opts.Dialogs.SetApp(n, false)
	}
}

// appInfo is the attachment as status says it; the web page's has no
// executable. Under mu.
func (s *server) appInfo(web bool) *daemonproto.AppInfo {
	if s.app == nil {
		return &daemonproto.AppInfo{}
	}
	a := &daemonproto.AppInfo{Attached: true, PID: s.app.conn.pid, Version: s.app.version, Since: s.app.since,
		Capabilities: slices.Clone(s.app.caps)}
	if !web {
		a.Executable = s.app.conn.exe
	}
	return a
}

// Replaced is replaced.json: the caboose a daemon exited for, by a
// replace, which a client that would start a daemon of its own reads
// first: it never brings back an older one.
type Replaced struct {
	// Version and Protocol are the replacing caboose's (a daemon's start
	// writes its own over a file it finds).
	Version  string    `json:"version"`
	Protocol int       `json:"protocol"`
	At       time.Time `json:"at"`
}

// maxReplaced bounds what ReadReplaced reads.
const maxReplaced = 4096

// ReadReplaced reads the daemon directory's replaced.json; an error
// satisfying errors.Is(err, fs.ErrNotExist) when there is none.
func ReadReplaced(dir string) (Replaced, error) {
	var r Replaced
	f, err := os.Open(filepath.Join(dir, ReplacedFile))
	if err != nil {
		return r, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxReplaced+1))
	if err != nil {
		return r, err
	}
	if len(b) > maxReplaced {
		return r, fmt.Errorf("%s is over %d bytes", ReplacedFile, maxReplaced)
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return r, fmt.Errorf("%s: %w", ReplacedFile, err)
	}
	r.Version = daemonproto.Text(r.Version, 64)
	return r, nil
}

// startReplaced writes this daemon over a replaced.json it finds: the
// newest daemon is the one that runs now, whichever it is.
func (s *server) startReplaced() {
	if _, err := os.Lstat(filepath.Join(s.opts.Dir, ReplacedFile)); errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err := s.writeJSON(ReplacedFile, Replaced{Version: s.opts.Version, Protocol: s.protocol(), At: s.started.UTC()}); err != nil {
		s.log.Printf("writing %s: %v", ReplacedFile, err)
	}
}

// exitReplaced writes replaced.json when a replace made the daemon stop,
// forced or not.
func (s *server) exitReplaced() {
	s.mu.Lock()
	r, by := s.result, s.replacedBy
	s.mu.Unlock()
	if !r.Replaced && !r.Forced {
		return
	}
	by.At = time.Now().UTC()
	if err := s.writeJSON(ReplacedFile, by); err != nil {
		s.log.Printf("writing %s: %v", ReplacedFile, err)
		return
	}
	s.log.Printf("wrote %s: replaced by caboose %s (protocol %d)", ReplacedFile, daemonproto.Text(by.Version, 64), by.Protocol)
}
