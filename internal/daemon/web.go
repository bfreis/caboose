package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/netip"
	"sync"
	"time"

	"github.com/bfreis/caboose/internal/daemonproto"
	"github.com/bfreis/caboose/internal/webui"
)

// WebOptions turn on the daemon's web page. Options.Web nil serves none,
// and makes no token.
//
// The token is made anew at every start and kept in the daemon's memory
// alone, never on disk: a token a page kept from before (or that
// something holding the port while the daemon was down read from a
// page's storage) lets nothing in once the daemon is back.
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

// newToken makes a token; a test makes it fail.
var newToken = daemonproto.NewToken

// webState is the daemon's side of its web page: the token and the
// one-time codes that open the page, both in memory only, and whether
// the page is served. Everything under mu.
type webState struct {
	want netip.AddrPort // where it is to be served (Options.Web's)
	now  func() time.Time
	log  *log.Logger

	mu        sync.Mutex
	token     string         // "" when none could be made: then nothing is let in
	addrErr   string         // set when want is refused
	tokenErr  string         // set when there is no token
	addr      netip.AddrPort // where it is served: the bound address, else want
	listening bool
	err       string // why it is not served
	since     time.Time
	codes     map[string]time.Time // outstanding one-time codes, by when each expires
}

// newWebState makes a new token, and starts not served.
func newWebState(o WebOptions, lg *log.Logger) *webState {
	w := &webState{want: o.Addr, addr: o.Addr, now: o.Now, log: lg, codes: map[string]time.Time{}}
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
	tok, err := newToken()
	if err != nil {
		lg.Printf("web: the page's token: %v", err)
		w.tokenErr = fmt.Sprintf("the page's token: %v", err)
		w.err = w.tokenErr
	} else {
		lg.Printf("web: made a new token for the page")
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

// rotate replaces the token with a new one: from when it returns, the old
// one lets nothing in, and neither does any code made before. On an error
// the old one stays.
// A rotation that makes a token after none could be had at the start
// clears that, and reports it (recovered): the page can be served now.
func (w *webState) rotate() (recovered bool, err error) {
	tok, err := newToken()
	if err != nil {
		return false, err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
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
			s.log.Printf("web: a new token could not be made: %v", err)
			return nil, nil, daemonproto.NewError(daemonproto.KindInternal, "a new token could not be made, so the old one stays: %v", err)
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
