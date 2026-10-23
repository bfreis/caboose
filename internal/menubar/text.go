package menubar

import (
	"fmt"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/bfreis/caboose/internal/daemonproto"
)

// MaxTitle is the most characters a menu title holds.
const MaxTitle = 80

// MaxSandboxSays is the most characters of a sandbox's own words an
// alert shows, label and all.
const MaxSandboxSays = 1000

// SandboxLabel heads the sandbox's own words wherever they are shown.
const SandboxLabel = "The sandbox says (not checked):"

const ellipsis = "…"

// Clean is s as one line of plain text: line breaks become spaces, and any
// other rune that is not printable (controls, format characters such as
// bidi overrides and zero-width ones, invalid UTF-8) becomes '?'. The
// daemon sends only printable text; this is defence in depth.
func Clean(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		i += n
		switch {
		case r == utf8.RuneError && n <= 1:
			b.WriteByte('?')
		case r == '\n' || r == '\r' || r == '\t' || r == '\v' || r == '\f' || r == 0x85 || r == 0x2028 || r == 0x2029:
			b.WriteByte(' ')
		case !unicode.IsPrint(r) && r != ' ':
			b.WriteByte('?')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Clip is Clean(s) cut to at most max characters, on a character
// boundary, ending in "…" when cut.
func Clip(s string, max int) string {
	s = Clean(s)
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	if max < 1 {
		return ""
	}
	rs := []rune(s)
	return string(rs[:max-1]) + ellipsis
}

func runes(s string) int { return utf8.RuneCountInString(s) }

// kindWord is how a request's kind reads in a title.
func kindWord(kind string) string {
	switch kind {
	case "alias":
		return "alias"
	case "egress":
		return "connect"
	case "open_url":
		return "open"
	case "host_exec":
		return "host command"
	}
	return Clean(kind)
}

// RequestTitle is a request's menu title, "#ID ENV KIND: SUBJECT", built
// from its structured fields and never by cutting its effect: SUBJECT is
// the alias name, the host it connects to, or the URL's origin followed
// by its path. When too long, only the middle of a URL's path gives way,
// never the host or the alias name; any other overlong title is cut at
// its end.
func RequestTitle(env string, v daemonproto.RequestView) string {
	head := fmt.Sprintf("#%d %s %s: ", v.ID, Clean(env), kindWord(v.Kind))
	kind, value, _ := strings.Cut(v.Key, ":")
	if kind != v.Kind {
		value = ""
	}
	value = Clean(value)
	switch {
	case v.Kind == "open_url" && value != "":
		origin, path := urlParts(value, v.Effect)
		budget := MaxTitle - runes(head) - runes(origin)
		if budget >= 0 {
			return head + origin + elidePath(path, budget)
		}
		return Clip(head+origin, MaxTitle)
	case value != "":
		return Clip(head+value, MaxTitle)
	}
	return Clip(strings.TrimSuffix(head, ": "), MaxTitle)
}

// urlParts is a URL request's origin (its key) and the path of the URL its
// effect names, when that URL has this very origin: the effect is host
// text, "open URL in the browser". Anything else gives no path.
func urlParts(origin, effect string) (string, string) {
	u, ok := strings.CutPrefix(effect, "open ")
	if ok {
		u, ok = strings.CutSuffix(u, " in the browser")
	}
	if !ok {
		return origin, ""
	}
	p, err := url.Parse(u)
	if err != nil || p.Scheme == "" || p.Host == "" || !strings.EqualFold(p.Scheme+"://"+p.Host, origin) {
		return origin, ""
	}
	path := Clean(p.EscapedPath())
	if path == "/" {
		path = ""
	}
	return origin, path
}

// elidePath fits path in budget characters by eliding its middle: its
// first and last segments kept around "…" when they fit, else as much of
// its start and end as fits.
func elidePath(path string, budget int) string {
	if runes(path) <= budget {
		return path
	}
	if budget < 2 {
		return ""
	}
	segs := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(segs) >= 2 {
		s := "/" + segs[0] + "/" + ellipsis + "/" + segs[len(segs)-1]
		if runes(s) <= budget {
			return s
		}
	}
	rs := []rune(path)
	keep := budget - 1
	front := (keep + 1) / 2
	back := keep - front
	return string(rs[:front]) + ellipsis + string(rs[len(rs)-back:])
}

// sandboxSays is a request's ask, the sandbox's own words, under
// SandboxLabel and within MaxSandboxSays; "" when it said nothing.
func sandboxSays(a *daemonproto.Ask, session string) string {
	var parts []string
	add := func(label, s string) {
		if s = strings.TrimSpace(Clean(s)); s != "" {
			parts = append(parts, label+s)
		}
	}
	if a != nil {
		add("", a.Context)
		if len(a.Options) > 0 {
			opts := make([]string, 0, len(a.Options))
			for _, o := range a.Options {
				if o = strings.TrimSpace(Clean(o)); o != "" {
					opts = append(opts, o)
				}
			}
			add("Options: ", strings.Join(opts, "; "))
		}
		add("Recommends: ", a.Recommendation)
		add("Meanwhile: ", a.Meanwhile)
	}
	add("Session: ", session)
	if len(parts) == 0 {
		return ""
	}
	return clipLines(SandboxLabel+"\n"+strings.Join(parts, "\n"), MaxSandboxSays)
}

// clipLines cuts s, which may hold line breaks the core put there, to at
// most max characters.
func clipLines(s string, max int) string {
	if runes(s) <= max {
		return s
	}
	rs := []rune(s)
	return string(rs[:max-1]) + ellipsis
}
