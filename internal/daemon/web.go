package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/bfreis/caboose/internal/daemonproto"
	"github.com/bfreis/caboose/internal/saferm"
	"github.com/bfreis/caboose/internal/webui"
)

// WebOptions turn on the daemon's web page. Options.Web nil serves none,
// and makes no token.
type WebOptions struct {
	// Addr is where the page is served: a loopback IPv4 address
	// (127.0.0.1:7171 for a release). Anything else is refused.
	Addr netip.AddrPort
	// Now is the clock; nil is time.Now.
	Now func() time.Time
}

// checkWebAddr refuses an address the page may not be served on: anything
// but an IPv4 loopback address.
func checkWebAddr(a netip.AddrPort) error { return webui.CheckAddr(a) }

// maxCodes is how many one-time codes may be outstanding at once: a new
// one past it drops the oldest.
const maxCodes = 16

// webState is the daemon's side of its web page: the token, which only
// it reads, the one-time codes that open the page (in memory only), and
// whether the page is served. Everything under mu.
type webState struct {
	dir  string
	want netip.AddrPort // where it is to be served (Options.Web's)
	now  func() time.Time
	log  *log.Logger

	mu        sync.Mutex
	token     string         // "" when none could be read or made: then nothing is let in
	addrErr   string         // set when want is refused
	tokenErr  string         // set when there is no token
	addr      netip.AddrPort // where it is served: the bound address, else want
	listening bool
	err       string // why it is not served
	since     time.Time
	codes     map[string]time.Time // outstanding one-time codes, by when each expires
}

// newWebState reads the directory's token, or makes one, and starts not
// served.
func newWebState(dir string, o WebOptions, lg *log.Logger) *webState {
	w := &webState{dir: dir, want: o.Addr, addr: o.Addr, now: o.Now, log: lg, codes: map[string]time.Time{}}
	if w.now == nil {
		w.now = time.Now
	}
	w.since = w.now()
	w.err = "the page is not served yet"
	if err := checkWebAddr(o.Addr); err != nil {
		w.addrErr = err.Error()
		w.err = w.addrErr
		lg.Printf("web: %v", err)
	}
	tok, err := loadToken(dir, lg)
	if err != nil {
		lg.Printf("web: the page's token: %v", err)
		w.tokenErr = fmt.Sprintf("the page's token: %v", err)
		w.err = w.tokenErr
	}
	w.token = tok
	return w
}

// current is the token, "" when there is none.
func (w *webState) current() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.token
}

// mintLocked makes a one-time code good for daemonproto.CodeTTL, dropping
// the expired ones and, past maxCodes, the oldest. Called under mu.
func (w *webState) mintLocked() (string, error) {
	c, err := daemonproto.NewCode()
	if err != nil {
		return "", err
	}
	now := w.now()
	for k, exp := range w.codes {
		if !now.Before(exp) {
			delete(w.codes, k)
		}
	}
	for len(w.codes) >= maxCodes {
		var oldest string
		var at time.Time
		for k, exp := range w.codes {
			if oldest == "" || exp.Before(at) {
				oldest, at = k, exp
			}
		}
		delete(w.codes, oldest)
	}
	w.codes[c] = now.Add(daemonproto.CodeTTL)
	return c, nil
}

// redeem trades a one-time code for the token: once, and before it
// expires. The code is gone after, whatever the answer. ok is false for
// a code of the wrong form, one never made or already used, one expired,
// and when there is no token; the caller tells none of these apart.
func (w *webState) redeem(code string) (string, bool) {
	if !daemonproto.ValidCode(code) {
		return "", false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	exp, ok := w.codes[code]
	delete(w.codes, code)
	if !ok || !w.now().Before(exp) || !daemonproto.ValidToken(w.token) {
		return "", false
	}
	return w.token, true
}

// rotate replaces the token with a new one, written first: from when it
// returns, the old one lets nothing in, and neither does any code made
// before. On an error the old one stays.
// A rotation that writes a token after none could be had at the start
// clears that, and reports it (recovered): the page can be served now.
func (w *webState) rotate() (recovered bool, err error) {
	tok, err := daemonproto.NewToken()
	if err != nil {
		return false, err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := writeToken(w.dir, tok); err != nil {
		return false, err
	}
	w.token = tok
	clear(w.codes)
	w.log.Printf("web: the page's token was replaced with a new one")
	if w.tokenErr != "" {
		w.tokenErr = ""
		if w.addrErr == "" && !w.listening {
			w.err = "the page is not served yet"
		}
		return true, nil
	}
	return false, nil
}

// setServed records whether the page is served, on what address it bound
// (unset when it is not), and why not; and reports whether that changed.
func (w *webState) setServed(listening bool, bound netip.AddrPort, why error) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	msg := ""
	if why != nil {
		msg = why.Error()
	}
	addr := w.want
	if listening && bound.IsValid() {
		addr = bound
	}
	if listening == w.listening && msg == w.err && addr == w.addr {
		return false
	}
	w.listening, w.err, w.addr, w.since = listening, msg, addr, w.now()
	return true
}

// servable reports whether the page can be served at all: an address it
// may be served on, and a token. An address refused stays refused while
// the daemon runs; a token comes back with a rotation that writes one.
func (w *webState) servable() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.addrErr == "" && w.tokenErr == "" && daemonproto.ValidToken(w.token)
}

// state is what status says of the page: never the token.
func (w *webState) state() daemonproto.WebState {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.stateLocked()
}

func (w *webState) stateLocked() daemonproto.WebState {
	return daemonproto.WebState{
		Listening: w.listening,
		Addr:      w.addr.String(),
		Error:     daemonproto.Text(w.err, 1024),
		Since:     w.since,
	}
}

// result is web's answer: the state, the address and, when code is set
// and there is a token, the URL of a fresh one-time code. Never the
// token.
func (w *webState) result(code bool) (*daemonproto.WebResult, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	r := &daemonproto.WebResult{WebState: w.stateLocked()}
	if w.addrErr != "" {
		return r, nil
	}
	r.URL = "http://" + w.addr.String() + "/"
	if code && daemonproto.ValidToken(w.token) {
		c, err := w.mintLocked()
		if err != nil {
			return nil, err
		}
		r.CodeURL = r.URL + "#code=" + c
	}
	return r, nil
}

// runWeb is web: the page's state and address, with a one-time code when
// asked for one; rotate replaces the token first.
func (s *server) runWeb(_ context.Context, _ caller, params json.RawMessage) (any, func(), *daemonproto.Error) {
	var p daemonproto.WebParams
	if e := decode(daemonproto.MethodWeb, params, &p); e != nil {
		return nil, nil, e
	}
	if s.web == nil {
		if p.Rotate {
			return nil, nil, daemonproto.NewError(daemonproto.KindBadRequest, "this daemon serves no web page, so it keeps no token")
		}
		return &daemonproto.WebResult{WebState: daemonproto.WebState{Error: "this daemon serves no web page"}}, nil, nil
	}
	if p.Rotate {
		recovered, err := s.web.rotate()
		if err != nil {
			s.log.Printf("web: a new token could not be written: %v", err)
			return nil, nil, daemonproto.NewError(daemonproto.KindInternal, "a new token could not be written, so the old one stays: %v", err)
		}
		if recovered {
			s.startWeb()
		}
	}
	r, err := s.web.result(p.Code)
	if err != nil {
		return nil, nil, daemonproto.NewError(daemonproto.KindInternal, "a code for the page could not be made: %v", err)
	}
	return r, nil, nil
}

// loadToken reads the daemon directory's web.token, or, when there is
// none or it cannot be trusted, makes a new one and writes it.
func loadToken(dir string, lg *log.Logger) (string, error) {
	tok, why := readToken(filepath.Join(dir, TokenFile), ownUID())
	if tok != "" {
		return tok, nil
	}
	tok, err := daemonproto.NewToken()
	if err != nil {
		return "", err
	}
	if err := writeToken(dir, tok); err != nil {
		return "", err
	}
	if why == "" {
		lg.Printf("web: made the page's token, %s", TokenFile)
	} else {
		lg.Printf("web: %s %s: replaced it with a new token", TokenFile, why)
	}
	return tok, nil
}

// readToken is the token in the file p, or "" and why it is not to be
// used ("" for a file that is not there). The file must be a regular
// file of uid's (the user's), mode 0600, holding a token and at most a newline;
// a symlink is never followed.
func readToken(p string, uid uint32) (string, string) {
	f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return "", ""
	}
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return "", "was a symlink"
		}
		return "", "could not be read"
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "", "could not be read"
	}
	if !fi.Mode().IsRegular() {
		return "", "was not a regular file"
	}
	if fi.Mode().Perm() != 0o600 {
		return "", fmt.Sprintf("had mode %#o, not 0600", fi.Mode().Perm())
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); !ok || st.Uid != uid {
		return "", "belonged to another user"
	}
	b, err := io.ReadAll(io.LimitReader(f, daemonproto.TokenLen+2))
	if err != nil {
		return "", "could not be read"
	}
	tok := string(bytes.TrimSuffix(b, []byte("\n")))
	if !daemonproto.ValidToken(tok) {
		return "", "did not hold a token"
	}
	return tok, ""
}

// writeToken writes tok to the daemon directory's web.token, 0600: to a
// temp file in the directory, renamed over it, so that a reader finds the
// old token or the new one, whole.
func writeToken(dir, tok string) error {
	f, err := os.CreateTemp(dir, "."+TokenFile+".*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	err = f.Chmod(0o600)
	if err == nil {
		_, err = f.WriteString(tok + "\n")
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, filepath.Join(dir, TokenFile))
	}
	if err != nil {
		_ = saferm.Remove(dir, tmp)
		return err
	}
	// The rename itself, kept: a rotated token never comes back as the
	// old one after a crash.
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = d.Sync()
	if cerr := d.Close(); err == nil {
		err = cerr
	}
	return err
}
