// Package webui is the host's caboose web page as HTTP: the handler, the
// checks every request passes before anything is served, the headers
// every response carries, the static files and the mapping of the API
// onto the daemon's methods. The page approves what a sandbox asks for,
// including answers that write config.toml, so these checks are what
// stands between any other web page, or any local process, and those
// writes: a request that fails one is refused before anything runs.
//
// It holds no state of the daemon's: every API call is one call of the
// daemon's method table, through Dispatcher, as the socket's are. It
// imports nothing of the daemon's or the launcher's.
package webui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/netip"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/bfreis/caboose/internal/daemonproto"
)

// Dispatcher runs the daemon's methods for the web page.
type Dispatcher interface {
	// Allowed reports whether the web page may call the method: the
	// daemon's method table lists it for a client of kind web.
	Allowed(method string) bool
	// Call runs one daemon method for a web client, exactly as the socket
	// would for a client of kind "web", and returns its result encoded.
	Call(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, *daemonproto.Error)
}

// Options configure Handler.
type Options struct {
	// Origin is the listener's own origin, from the address it bound:
	// "http://127.0.0.1:7171" (or a test listener's). Anything but
	// http:// and an IPv4 loopback address with a port refuses every
	// request.
	Origin string
	// Token is the current token, "" when there is none (then no API
	// call is let in). Rotation swaps what it returns.
	Token func() string
	// Redeem trades a one-time code for the token: once, within its
	// time, and false for anything else (POST /api/redeem). nil redeems
	// nothing.
	Redeem func(code string) (string, bool)
	// Dispatch runs the API's methods.
	Dispatch Dispatcher
	// Activity, when set, runs for every API request whose token was
	// right: the daemon's idle clock.
	Activity func()
	// Log is where refusals are counted (at most a line a minute, never
	// a header's value); nil logs nothing.
	Log *log.Logger
	// Now is the clock of the rate limits; nil is time.Now.
	Now func() time.Time
}

// TokenHeader is the request header the page sends its token in.
const TokenHeader = "X-Caboose-Token"

// MaxBody is the most an API request's body may hold.
const MaxBody = 64 << 10

// Rate limits: authenticated calls across the daemon, proofs, and
// redeemed codes. A proof needs no token, so its limit is there against
// something that hammers /api/proof; it equals the API's because the
// page proves the server again before every call that carries the
// token, one proof a call, and a lower one would hold the page's calls
// under it. A code is redeemed once each time the page is opened, so
// its limit is low: against guessing, which 256 bits make hopeless
// anyway.
const (
	apiRate     = 20
	apiBurst    = 40
	proofRate   = apiRate
	proofBurst  = apiBurst
	redeemRate  = 1
	redeemBurst = 5
)

// The kinds of a refusal by the checks, the web page's alone: the socket
// never sends them. A wrong Host (421) is answered in plain text, since
// it is no request of the page's.
const (
	KindForbidden        = "forbidden"
	KindUnauthorized     = "unauthorized"
	KindUnsupportedMedia = "unsupported_media"
	KindTooMany          = "too_many"
	KindMethodNotAllowed = "method_not_allowed"
)

// Headers is what every response carries, whatever it answers.
var Headers = [][2]string{
	{"Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src 'self'; font-src 'none'; form-action 'none'; frame-ancestors 'none'; base-uri 'none'; manifest-src 'none'; worker-src 'none'; require-trusted-types-for 'script'; trusted-types 'none'"},
	{"X-Frame-Options", "DENY"},
	{"X-Content-Type-Options", "nosniff"},
	{"Referrer-Policy", "no-referrer"},
	{"Cross-Origin-Opener-Policy", "same-origin"},
	{"Cross-Origin-Resource-Policy", "same-origin"},
	{"Permissions-Policy", "camera=(), microphone=(), geolocation=(), usb=(), serial=(), hid=(), bluetooth=(), payment=(), clipboard-read=(), display-capture=()"},
	{"Cache-Control", "no-store"},
}

// Content types.
const (
	typeHTML = "text/html; charset=utf-8"
	typeCSS  = "text/css; charset=utf-8"
	typeJS   = "text/javascript; charset=utf-8"
	typeJSON = "application/json; charset=utf-8"
	typeText = "text/plain; charset=utf-8"
)

// methodRe is the form of an API method's name.
var methodRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// The API paths that take no token: the proof, and the code's redeem.
const (
	proofMethod  = "proof"
	redeemMethod = "redeem"
)

// The messages a refusal says.
const (
	msgUnauthorized = "the page's token is missing or out of date: run `caboose ui`"
	msgBrowser      = "this browser sends no Sec-Fetch-Site: use a current Safari, Chrome or Firefox"
	// msgBadCode is the one answer to every code that opens nothing:
	// wrong, of the wrong form, expired or used.
	msgBadCode = "this address's code is used or out of date: run `caboose ui` again"
)

// KindBadCode is the kind of every refused redeem, whatever was wrong
// with the code.
const KindBadCode = "bad_code"

type handler struct {
	o       Options
	origin  string // "" when Options.Origin is unusable: every request refused
	host    string // the origin's host and port, the one Host taken
	now     func() time.Time
	api     *bucket
	proofs  *bucket
	redeems *bucket
	fails   *failLog
}

// Handler is the web page: GET / and the static files, POST /api/proof,
// POST /api/redeem, and POST /api/METHOD for each method the daemon lets the page call.
func Handler(o Options) http.Handler {
	h := &handler{o: o, now: o.Now}
	if h.now == nil {
		h.now = time.Now
	}
	if host, ok := originHost(o.Origin); ok && o.Token != nil && o.Dispatch != nil {
		h.origin, h.host = o.Origin, host
	} else if o.Log != nil {
		o.Log.Printf("web: the page refuses every request: its origin %q is not http:// and an IPv4 loopback address, or it has no token or dispatcher", daemonproto.Text(o.Origin, 64))
	}
	h.api = newBucket(apiRate, apiBurst, h.now)
	h.proofs = newBucket(proofRate, proofBurst, h.now)
	h.redeems = newBucket(redeemRate, redeemBurst, h.now)
	h.fails = &failLog{log: o.Log, now: h.now}
	return h
}

// originHost is the host and port of an origin the page may have:
// http://, an IPv4 loopback address and a port, nothing else.
func originHost(origin string) (string, bool) {
	rest, ok := strings.CutPrefix(origin, "http://")
	if !ok {
		return "", false
	}
	a, err := netip.ParseAddrPort(rest)
	if err != nil || CheckAddr(a) != nil || a.Port() == 0 || a.String() != rest {
		return "", false
	}
	return rest, true
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	hd := w.Header()
	for _, kv := range Headers {
		hd.Set(kv[0], kv[1])
	}
	if h.origin == "" {
		h.fail(w, http.StatusInternalServerError, daemonproto.KindInternal, "the page is not set up", "")
		return
	}

	// 1. Host: the listener's own address, as the browser wrote it when
	// it opened the page. A DNS name rebound to 127.0.0.1 sends its own.
	// The request line must be a path: an absolute URL would stand in
	// for the Host.
	if !strings.HasPrefix(r.RequestURI, "/") || !strings.EqualFold(r.Host, h.host) {
		h.fails.add("host")
		hd.Set("Content-Type", typeText)
		w.WriteHeader(http.StatusMisdirectedRequest)
		io.WriteString(w, "open "+h.origin+"/\n")
		return
	}
	get := r.Method == http.MethodGet || r.Method == http.MethodHead
	root := r.URL.Path == "/"

	// 2. Sec-Fetch-Site, when sent: the page's own, or none (typed, or
	// opened from outside the browser) for the page itself. same-site
	// would be another port of 127.0.0.1, such as a forwarded sandbox's.
	if site := r.Header.Values("Sec-Fetch-Site"); len(site) > 1 ||
		len(site) == 1 && site[0] != "same-origin" && !(site[0] == "none" && root && get) {
		h.fail(w, http.StatusForbidden, KindForbidden, "refused: the request does not come from the page itself", "fetch-site")
		return
	}
	// 3. Sec-Fetch-Dest: never into a frame or an embedding element.
	if dest := r.Header.Values("Sec-Fetch-Dest"); len(dest) > 1 || len(dest) == 1 && framed(dest[0]) {
		h.fail(w, http.StatusForbidden, KindForbidden, "refused: the page is never shown inside another", "fetch-dest")
		return
	}

	switch p := r.URL.Path; {
	case root:
		if !get {
			h.notAllowed(w, "GET, HEAD")
			return
		}
		h.serveAsset(w, indexAsset)
	case strings.HasPrefix(p, "/static/"):
		a, ok := assets[strings.TrimPrefix(p, "/static/")]
		if !ok || r.URL.RawPath != "" {
			h.notFound(w)
			return
		}
		if !get {
			h.notAllowed(w, "GET, HEAD")
			return
		}
		h.serveAsset(w, a)
	case strings.HasPrefix(p, "/api/"):
		h.serveAPI(w, r, strings.TrimPrefix(p, "/api/"))
	default:
		h.notFound(w)
	}
}

// framed reports whether a Sec-Fetch-Dest puts the response in a frame
// or an embedding element.
func framed(dest string) bool {
	switch strings.ToLower(dest) {
	case "iframe", "frame", "object", "embed", "fencedframe":
		return true
	}
	return false
}

func (h *handler) serveAPI(w http.ResponseWriter, r *http.Request, name string) {
	// 4. Every API call is a POST: OPTIONS included, so no preflight is
	// ever answered, and no Access-Control-* header is ever sent.
	if r.Method != http.MethodPost {
		h.notAllowed(w, http.MethodPost)
		return
	}
	// 5. Origin: exactly the page's own.
	if o := r.Header.Values("Origin"); len(o) != 1 || o[0] != h.origin {
		h.fail(w, http.StatusForbidden, KindForbidden, "refused: the request does not come from the page itself", "origin")
		return
	}
	// 6. Sec-Fetch-Site: sent, and the page's own.
	switch site := r.Header.Values("Sec-Fetch-Site"); {
	case len(site) == 0:
		h.fail(w, http.StatusForbidden, KindForbidden, msgBrowser, "fetch-site")
		return
	case len(site) != 1 || site[0] != "same-origin":
		h.fail(w, http.StatusForbidden, KindForbidden, "refused: the request does not come from the page itself", "fetch-site")
		return
	}
	// 7. Content-Type: JSON, which no form sends and no cross-origin
	// request sends without a preflight.
	if !jsonType(r.Header.Values("Content-Type")) {
		h.fail(w, http.StatusUnsupportedMediaType, KindUnsupportedMedia, "the body must be application/json", "content-type")
		return
	}
	proof := name == proofMethod
	redeem := name == redeemMethod
	// 8. The token, in its header only, in constant time.
	if !proof && !redeem {
		tok := r.Header.Values(TokenHeader)
		if len(tok) != 1 || !daemonproto.TokenEqual(tok[0], h.o.Token()) {
			h.fail(w, http.StatusUnauthorized, KindUnauthorized, msgUnauthorized, "token")
			return
		}
		if h.o.Activity != nil {
			h.o.Activity()
		}
	}
	// 9. A method of the page's.
	if !methodRe.MatchString(name) || r.URL.RawPath != "" || !proof && !redeem && !h.o.Dispatch.Allowed(name) {
		h.fail(w, http.StatusNotFound, daemonproto.KindUnknownMethod, fmt.Sprintf("no method %q for the page", daemonproto.Text(name, 64)), "")
		return
	}
	// 10. No query, and a body of one JSON object, bounded.
	if r.URL.RawQuery != "" || r.URL.ForceQuery {
		h.fail(w, http.StatusBadRequest, daemonproto.KindBadRequest, "the API takes no query string", "")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBody))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			h.fail(w, http.StatusBadRequest, daemonproto.KindBadRequest, fmt.Sprintf("the body is over %d bytes", MaxBody), "")
		} else {
			h.fail(w, http.StatusBadRequest, daemonproto.KindBadRequest, "the body could not be read", "")
		}
		return
	}
	params, ok := oneObject(body)
	if !ok {
		h.fail(w, http.StatusBadRequest, daemonproto.KindBadRequest, "the body must be one JSON object", "")
		return
	}
	// 11. Rate limits.
	limit := h.api
	switch {
	case proof:
		limit = h.proofs
	case redeem:
		limit = h.redeems
	}
	if !limit.allow() {
		h.fail(w, http.StatusTooManyRequests, KindTooMany, "too many requests: try again in a moment", "")
		return
	}

	if proof {
		h.serveProof(w, params)
		return
	}
	if redeem {
		h.serveRedeem(w, params)
		return
	}
	res, e := h.o.Dispatch.Call(r.Context(), name, params)
	if e != nil {
		status, kind := dispatchStatus(e.Kind)
		h.fail(w, status, kind, e.Message, "")
		return
	}
	if len(bytes.TrimSpace(res)) == 0 {
		res = json.RawMessage("{}")
	}
	h.writeJSON(w, http.StatusOK, struct {
		Result json.RawMessage `json:"result"`
	}{res})
}

// ProofParams is /api/proof's body.
type ProofParams struct {
	Nonce string `json:"nonce"`
}

// ProofResult is /api/proof's result.
type ProofResult struct {
	MAC string `json:"mac"`
}

// serveProof answers a proof: the token's MAC of the listener's own
// origin, never one a request names, and the page's nonce.
func (h *handler) serveProof(w http.ResponseWriter, params json.RawMessage) {
	var p ProofParams
	d := json.NewDecoder(bytes.NewReader(params))
	d.DisallowUnknownFields()
	if err := d.Decode(&p); err != nil || !daemonproto.ValidNonce(p.Nonce) {
		h.fail(w, http.StatusBadRequest, daemonproto.KindBadRequest, fmt.Sprintf("the nonce must be %d base64url characters", daemonproto.NonceLen), "")
		return
	}
	tok := h.o.Token()
	if !daemonproto.ValidToken(tok) {
		h.fail(w, http.StatusServiceUnavailable, daemonproto.KindUnavailable, "the daemon has no token for the page", "")
		return
	}
	h.writeJSON(w, http.StatusOK, struct {
		Result ProofResult `json:"result"`
	}{ProofResult{MAC: daemonproto.WebProof(tok, h.origin, p.Nonce)}})
}

// RedeemParams is /api/redeem's body.
type RedeemParams struct {
	Code string `json:"code"`
}

// RedeemResult is /api/redeem's result.
type RedeemResult struct {
	Token string `json:"token"`
}

// serveRedeem trades a one-time code for the token. Every code that
// opens nothing, a body that names none included, gets the same answer,
// so a try learns nothing of why. Neither the code nor the token is
// logged.
func (h *handler) serveRedeem(w http.ResponseWriter, params json.RawMessage) {
	var p RedeemParams
	d := json.NewDecoder(bytes.NewReader(params))
	d.DisallowUnknownFields()
	tok, ok := "", false
	if d.Decode(&p) == nil && daemonproto.ValidCode(p.Code) && h.o.Redeem != nil {
		tok, ok = h.o.Redeem(p.Code)
	}
	if !ok || !daemonproto.ValidToken(tok) {
		h.fail(w, http.StatusUnauthorized, KindBadCode, msgBadCode, "code")
		return
	}
	if h.o.Activity != nil {
		h.o.Activity()
	}
	h.writeJSON(w, http.StatusOK, struct {
		Result RedeemResult `json:"result"`
	}{RedeemResult{Token: tok}})
}

// jsonType reports whether a request's Content-Type is JSON: exactly one
// header, application/json, with at most a charset of utf-8.
func jsonType(v []string) bool {
	if len(v) != 1 {
		return false
	}
	switch strings.ReplaceAll(strings.ToLower(v[0]), " ", "") {
	case "application/json", "application/json;charset=utf-8":
		return true
	}
	return false
}

// oneObject is the body as params: one JSON object and nothing after it,
// or {} for an empty body.
func oneObject(b []byte) (json.RawMessage, bool) {
	b = bytes.TrimSpace(b)
	if len(b) == 0 {
		return json.RawMessage("{}"), true
	}
	if b[0] != '{' || !json.Valid(b) {
		return nil, false
	}
	return json.RawMessage(b), true
}

// dispatchStatus is the HTTP status of a daemon error, and the kind the
// page is told: a method the page may not call is one there is not.
func dispatchStatus(kind string) (int, string) {
	switch kind {
	case daemonproto.KindBadRequest:
		return http.StatusBadRequest, kind
	case daemonproto.KindNotAllowed:
		return http.StatusNotFound, daemonproto.KindUnknownMethod
	case daemonproto.KindUnknownMethod, daemonproto.KindUnknownEnv, daemonproto.KindNotFound:
		return http.StatusNotFound, kind
	case daemonproto.KindAlreadyAnswered, daemonproto.KindStaleDiff, daemonproto.KindNotOwner:
		return http.StatusConflict, kind
	case daemonproto.KindBusy, daemonproto.KindBrokerUnavailable:
		return http.StatusServiceUnavailable, kind
	case daemonproto.KindTimeout:
		return http.StatusGatewayTimeout, kind
	case daemonproto.KindProtocolMismatch, daemonproto.KindUnavailable, daemonproto.KindBadFrame, daemonproto.KindInternal:
		return http.StatusInternalServerError, kind
	}
	return http.StatusInternalServerError, daemonproto.KindInternal
}

func (h *handler) serveAsset(w http.ResponseWriter, a asset) {
	w.Header().Set("Content-Type", a.typ)
	w.WriteHeader(http.StatusOK)
	w.Write(a.body)
}

func (h *handler) notFound(w http.ResponseWriter) {
	h.fail(w, http.StatusNotFound, daemonproto.KindNotFound, "no such page", "")
}

func (h *handler) notAllowed(w http.ResponseWriter, allow string) {
	w.Header().Set("Allow", allow)
	h.fail(w, http.StatusMethodNotAllowed, KindMethodNotAllowed, "method not allowed", "")
}

// fail answers a refusal or an error, and counts a refused check.
func (h *handler) fail(w http.ResponseWriter, status int, kind, msg, check string) {
	if check != "" {
		h.fails.add(check)
	}
	h.writeJSON(w, status, struct {
		Error daemonproto.Error `json:"error"`
	}{daemonproto.Error{Kind: kind, Message: daemonproto.Text(msg, 1024)}})
}

func (h *handler) writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		status = http.StatusInternalServerError
		b = []byte(`{"error":{"kind":"internal","message":"the answer could not be encoded"}}`)
	}
	w.Header().Set("Content-Type", typeJSON)
	w.WriteHeader(status)
	w.Write(append(b, '\n'))
}

// bucket is a token bucket: rate a second, at most burst at once.
type bucket struct {
	mu     sync.Mutex
	rate   float64
	burst  float64
	tokens float64
	last   time.Time
	now    func() time.Time
}

func newBucket(rate, burst float64, now func() time.Time) *bucket {
	return &bucket{rate: rate, burst: burst, tokens: burst, last: now(), now: now}
}

func (b *bucket) allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	if d := now.Sub(b.last); d > 0 {
		b.tokens = min(b.burst, b.tokens+d.Seconds()*b.rate)
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// failLog counts refused checks and logs them at most once a minute, by
// check, never with what a header held.
type failLog struct {
	log    *log.Logger
	now    func() time.Time
	mu     sync.Mutex
	last   time.Time
	counts map[string]int
}

func (f *failLog) add(check string) {
	if f.log == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.counts == nil {
		f.counts = map[string]int{}
	}
	f.counts[check]++
	now := f.now()
	if !f.last.IsZero() && now.Sub(f.last) < time.Minute {
		return
	}
	var parts []string
	for _, c := range []string{"host", "fetch-site", "fetch-dest", "origin", "content-type", "token", "code"} {
		if n := f.counts[c]; n > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", c, n))
		}
	}
	f.log.Printf("web: refused requests since the last report, by check: %s", strings.Join(parts, ", "))
	f.counts = map[string]int{}
	f.last = now
}
