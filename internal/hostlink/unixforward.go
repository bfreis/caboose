package hostlink

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"syscall"
	"time"

	"github.com/bfreis/caboose/internal/agentproto"
	"github.com/bfreis/caboose/internal/saferm"
)

// A socket forward lets a program on this machine reach a Unix socket that
// listens in the sandbox: it connects to a socket here, and the link carries
// the connection. Which sockets is [link.forward_sockets] in config.toml and
// nothing else. The sandbox can only be connected to: it neither names a path
// nor causes a listener, beyond saying in its hello that its agent serves
// them, and the socket here is mode 0600 in a directory of mode 0700, so only
// this machine's own user reaches it.

const (
	// MaxSocketPath is the longest path a socket here may have, on every
	// host: a Mac's sun_path holds 104 bytes, with its NUL.
	MaxSocketPath = 103
	// maxUnixConns is the most connections the socket forwards of a link
	// carry at once, together: well under agentproto.MaxStreams, which the
	// link's other streams share.
	maxUnixConns = 64
	// unixLogEvery is how often a refused connection is logged, at most.
	unixLogEvery = 30 * time.Second
	// ReasonSocketOldAgent is why no socket here listens for an agent whose
	// hello does not say it serves them.
	ReasonSocketOldAgent = "this sandbox's caboose-agent is too old to serve it: 'caboose restart' recreates the sandbox from this caboose's image"
)

// UnixForward is one socket forward.
type UnixForward struct {
	Name string
	// Sandbox is the socket in the sandbox, Host the one here, whose
	// directory holds nothing but these sockets.
	Sandbox, Host string
}

type unixState struct {
	listeners []unixListener
	conns     int
	lastLog   time.Time
	wg        sync.WaitGroup
}

type unixListener struct {
	ln   *net.UnixListener
	path string
	// own is the socket as this listener made it.
	own os.FileInfo
}

// CheckSocketPath says why path cannot be a socket here, and what to do.
func CheckSocketPath(path string) error {
	if len(path) > MaxSocketPath {
		return fmt.Errorf("its socket's path, %s, is %d bytes, and a Unix socket's can have at most %d: use a shorter name in [link.forward_sockets] of config.toml, or set CABOOSE_HOME to a shorter path",
			path, len(path), MaxSocketPath)
	}
	return nil
}

// startUnixForwards listens for each socket forward when the agent serves
// them, and says what came of each. One that cannot listen leaves the others
// and the link be.
func (h *Host) startUnixForwards(agentServes bool) {
	if len(h.cfg.UnixForwards) == 0 {
		return
	}
	infos := make([]agentproto.SocketInfo, 0, len(h.cfg.UnixForwards))
	for _, f := range h.cfg.UnixForwards {
		info := agentproto.SocketInfo{Name: f.Name, Path: f.Sandbox, HostPath: f.Host}
		if !agentServes {
			// No listener; only what a dead link left, not to refuse clients.
			if CheckSocketPath(f.Host) == nil {
				_ = clearStaleSocket(f.Host)
			}
			info.Reason = ReasonSocketOldAgent
			h.cfg.Log.Printf("socket %s: not listening: %s", f.Name, info.Reason)
			infos = append(infos, info)
			continue
		}
		l, err := listenUnixForward(f.Host)
		if err != nil {
			info.Reason = err.Error()
			h.cfg.Log.Printf("socket %s: not listening: %s", f.Name, info.Reason)
			infos = append(infos, info)
			continue
		}
		h.mu.Lock()
		h.unix.listeners = append(h.unix.listeners, l)
		h.unix.wg.Add(1)
		h.mu.Unlock()
		h.cfg.Log.Printf("socket %s: %s reaches %s in the sandbox", f.Name, f.Host, f.Sandbox)
		go h.serveUnixForward(f, l.ln)
		infos = append(infos, info)
	}
	if h.cfg.UnixForwarded != nil {
		h.cfg.UnixForwarded(slices.Clone(infos))
	}
	if agentServes {
		if err := h.sess.Send(agentproto.Message{Type: agentproto.TypeSockets, Sockets: infos}); err != nil {
			h.cfg.Log.Printf("cannot tell the agent of the socket forwards: %v", err)
		}
	}
}

// listenUnixForward makes the socket at path, in its directory of mode 0700,
// mode 0600, replacing only what clearStaleSocket finds dead.
func listenUnixForward(path string) (unixListener, error) {
	if err := CheckSocketPath(path); err != nil {
		return unixListener{}, err
	}
	dir := filepath.Dir(path)
	if err := socketDir(dir); err != nil {
		return unixListener{}, err
	}
	if err := clearStaleSocket(path); err != nil {
		return unixListener{}, err
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return unixListener{}, fmt.Errorf("cannot listen at %s: %v; 'caboose link --restart' tries again", path, err)
	}
	// Removed by removeOwnSocket, rather than by the listener's Close.
	ln.SetUnlinkOnClose(false)
	own, err := os.Lstat(path)
	if err == nil {
		err = os.Chmod(path, 0o600)
	}
	if err != nil {
		ln.Close()
		if own != nil {
			_ = removeOwnSocket(path, own)
		}
		return unixListener{}, fmt.Errorf("cannot make %s private: %v; 'caboose link --restart' tries again", path, err)
	}
	return unixListener{ln: ln, path: path, own: own}, nil
}

// socketDir makes dir, mode 0700, or refuses what is at its place.
func socketDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("cannot make %s: %v; fix that, then 'caboose link --restart'", dir, err)
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("cannot read %s: %v; fix that, then 'caboose link --restart'", dir, err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a directory: move it away, then 'caboose link --restart'", dir)
	}
	if fi.Mode().Perm() != 0o700 {
		if err := os.Chmod(dir, 0o700); err != nil {
			return fmt.Errorf("cannot make %s private: %v; fix that, then 'caboose link --restart'", dir, err)
		}
	}
	return nil
}

// clearStaleSocket removes the socket at path if it is dead: a connection to
// it is refused. link.lock is per data dir, so a link with another
// CABOOSE_DATA_DIR on this environment can be listening there, and one that
// answers is left alone, an error. So is anything that is no socket: this
// never follows a link.
func clearStaleSocket(path string) error {
	fi, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("cannot read %s: %v; fix that, then 'caboose link --restart'", path, err)
	}
	if fi.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("%s is there and is not a socket, so it is left in place: move it away, then 'caboose link --restart'", path)
	}
	c, err := net.DialTimeout("unix", path, time.Second)
	if err == nil {
		c.Close()
		return fmt.Errorf("something already answers at %s, so it is left alone: most likely a link of this environment started with another CABOOSE_DATA_DIR; stop that, then 'caboose link --restart'", path)
	}
	if !socketIsDead(err) {
		return fmt.Errorf("cannot tell whether anything listens at %s (%v), so it is left alone: fix that, then 'caboose link --restart'", path, err)
	}
	if err := saferm.Remove(filepath.Dir(path), path); err != nil {
		return fmt.Errorf("cannot replace the old socket %s: %v; remove it, then 'caboose link --restart'", path, err)
	}
	return nil
}

// socketIsDead says whether a dial's err shows nothing listens: a full backlog
// (EAGAIN), or anything else, does not.
func socketIsDead(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, os.ErrNotExist)
}

// removeOwnSocket removes the socket at path if it is still the one own
// describes, by device and inode: never one that took its place. What it
// cannot remove is only a stale socket, which the next link replaces.
func removeOwnSocket(path string, own os.FileInfo) error {
	fi, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSocket == 0 || !os.SameFile(fi, own) {
		return nil
	}
	return saferm.Remove(filepath.Dir(path), path)
}

// serveUnixForward connects each connection to f's socket here to a new
// stream, whose header names the socket in the sandbox.
func (h *Host) serveUnixForward(f UnixForward, ln *net.UnixListener) {
	defer h.unix.wg.Done()
	hdr, _ := json.Marshal(agentproto.StreamHeader{Socket: f.Sandbox})
	for {
		c, err := ln.AcceptUnix()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			// Out of descriptors, say: the forward stays, and tries again.
			h.unixLogf("socket %s: %v", f.Name, err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		if !h.takeUnixConn() {
			c.Close()
			h.unixLogf("socket %s: a connection refused: already %d open through the link's socket forwards", f.Name, maxUnixConns)
			continue
		}
		st, err := h.sess.Open(hdr)
		if err != nil {
			h.releaseUnixConn()
			c.Close()
			h.unixLogf("socket %s: a connection refused: %v", f.Name, err)
			continue
		}
		go func() {
			defer h.releaseUnixConn()
			agentproto.Splice(st, c)
		}()
	}
}

func (h *Host) takeUnixConn() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.unix.conns >= maxUnixConns {
		return false
	}
	h.unix.conns++
	return true
}

func (h *Host) releaseUnixConn() {
	h.mu.Lock()
	h.unix.conns--
	h.mu.Unlock()
}

// unixLogf logs, at most once per unixLogEvery.
func (h *Host) unixLogf(format string, args ...any) {
	h.mu.Lock()
	now := time.Now()
	if !h.unix.lastLog.IsZero() && now.Sub(h.unix.lastLog) < unixLogEvery {
		h.mu.Unlock()
		return
	}
	h.unix.lastLog = now
	h.mu.Unlock()
	h.cfg.Log.Printf(format, args...)
}

// closeUnixForwards stops listening and removes the sockets here. The
// connections they carried end with the session.
func (h *Host) closeUnixForwards() {
	h.mu.Lock()
	ls := h.unix.listeners
	h.unix.listeners = nil
	h.mu.Unlock()
	if len(h.cfg.UnixForwards) == 0 {
		return
	}
	for _, l := range ls {
		l.ln.Close()
		if err := removeOwnSocket(l.path, l.own); err != nil {
			h.cfg.Log.Printf("socket: %v", err)
		}
	}
	h.unix.wg.Wait()
	if h.cfg.UnixForwarded != nil {
		h.cfg.UnixForwarded(nil)
	}
}
