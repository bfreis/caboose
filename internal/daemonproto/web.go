package daemonproto

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"strings"
	"time"
)

// MethodWeb is the web page's state and address, with a one-time code
// that opens it when asked for: see WebParams and WebResult. Only a
// socket client's (the command line, the app): never the web page's own.
const MethodWeb = "web"

// The web page's token: 32 random bytes, base64url without padding.
const (
	TokenBytes = 32
	TokenLen   = 43 // base64.RawURLEncoding.EncodedLen(TokenBytes)
)

// The one-time code that opens the page: 32 random bytes, base64url
// without padding. The page trades it, once and within CodeTTL, for the
// token (POST /api/redeem), so the token itself is never in an address.
const (
	CodeBytes = 32
	CodeLen   = 43 // base64.RawURLEncoding.EncodedLen(CodeBytes)
	CodeTTL   = 60 * time.Second
)

// NonceLen is the length of a proof's nonce: 32 bytes, base64url without
// padding, like a token.
const NonceLen = TokenLen

// ProofDomain starts every message a proof is a MAC of, so that the MAC
// of a chosen nonce is of no use for anything else.
const ProofDomain = "caboose-web-proof-v1\n"

// NewToken returns a fresh token from the system's random source.
func NewToken() (string, error) {
	b := make([]byte, TokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// NewCode returns a fresh one-time code from the system's random source.
func NewCode() (string, error) {
	b := make([]byte, CodeBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// ValidCode reports whether s has a one-time code's form: exactly CodeLen
// characters of [A-Za-z0-9_-].
func ValidCode(s string) bool { return len(s) == CodeLen && base64URL(s) }

// ValidToken reports whether s has a token's form: exactly TokenLen
// characters of [A-Za-z0-9_-].
func ValidToken(s string) bool { return len(s) == TokenLen && base64URL(s) }

// ValidNonce reports whether s has a proof nonce's form: exactly NonceLen
// characters of [A-Za-z0-9_-].
func ValidNonce(s string) bool { return len(s) == NonceLen && base64URL(s) }

func base64URL(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !('A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// TokenEqual reports whether got is the token want, in time that does not
// depend on where they differ. A got of another length, or a want that is
// no token, is refused without comparing.
func TokenEqual(got, want string) bool {
	if len(got) != TokenLen || !ValidToken(want) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// WebProof is what the daemon answers a page's proof request with: the
// HMAC-SHA256, keyed with the token's ASCII bytes, of ProofDomain, the
// page's origin, a newline and the page's nonce, in base64url without
// padding. The page computes the same and sends the token only to a
// server whose answer matches: one that knows it.
func WebProof(token, origin, nonce string) string {
	m := hmac.New(sha256.New, []byte(token))
	m.Write([]byte(ProofDomain + origin + "\n" + nonce))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// WebParams are web's params.
type WebParams struct {
	// Rotate replaces the token with a new one, which takes effect at
	// once: a page holding the old one is refused from then on.
	Rotate bool `json:"rotate,omitempty"`
	// Code asks for a one-time code, in WebResult.CodeURL: what opens
	// the page. Without it, none is made.
	Code bool `json:"code,omitempty"`
}

// Validate checks the params (there is nothing to check).
func (p *WebParams) Validate() error { return nil }

// WebState is what the daemon says of its web page, in status: never the
// token.
type WebState struct {
	// Listening is set while the page is served.
	Listening bool `json:"listening"`
	// Addr is the address it is served on, or would be:
	// 127.0.0.1:PORT.
	Addr string `json:"addr"`
	// Error is why it is not served, when it is not.
	Error string `json:"error,omitempty"`
	// Since is when it entered its state.
	Since time.Time `json:"since,omitzero"`
}

func (w *WebState) validate() error {
	if err := checkText("web address", w.Addr, maxName); err != nil {
		return err
	}
	return checkText("web error", w.Error, maxMessage)
}

// WebResult is the result of web: the page's state and how to open it.
type WebResult struct {
	WebState
	// URL is the page's address, http://127.0.0.1:PORT/.
	URL string `json:"url,omitempty"`
	// CodeURL is URL with a fresh one-time code in its fragment,
	// #code=C: what caboose ui and the app open. Set only when asked
	// for (WebParams.Code) and the daemon has a token; it opens the page
	// once, within CodeTTL.
	CodeURL string `json:"code_url,omitempty"`
}

// Validate checks the result: its texts, and that CodeURL is URL with a
// code's fragment and nothing else.
func (r *WebResult) Validate() error {
	if err := r.WebState.validate(); err != nil {
		return err
	}
	if err := checkText("web url", r.URL, maxName); err != nil {
		return err
	}
	if r.URL != "" && (!strings.HasPrefix(r.URL, "http://") || !strings.HasSuffix(r.URL, "/")) {
		return bad("bad web url")
	}
	if r.CodeURL == "" {
		return nil
	}
	if r.URL == "" || r.Code() == "" {
		return bad("bad web code url")
	}
	return nil
}

// Code is the one-time code in CodeURL, or "".
func (r *WebResult) Code() string {
	c, ok := strings.CutPrefix(r.CodeURL, r.URL+"#code=")
	if !ok || r.URL == "" || !ValidCode(c) {
		return ""
	}
	return c
}

// Web asks for the web page's state and address: with a one-time code
// when p.Code, the token replaced first when p.Rotate.
func (c *Client) Web(ctx context.Context, p WebParams) (*WebResult, error) {
	var r WebResult
	if err := c.Call(ctx, MethodWeb, &p, &r); err != nil {
		return nil, err
	}
	return &r, nil
}
