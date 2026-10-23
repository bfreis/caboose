package config

import (
	"errors"
	"fmt"
	"maps"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// The trust levels: what the sandbox is assumed able to do. A fallible
// sandbox is an agent that may make mistakes; a hostile one is steered by
// someone else.
const (
	TrustFallible = "fallible"
	TrustHostile  = "hostile"
)

// aliasKey is the family of [alias.NAME] tables.
const aliasKey = "alias"

// aliasKeys are the keys of an [alias.NAME] table; params is a sub-table
// of arrays, read apart.
var aliasKeys = map[string]valueType{
	"command":     typeStrings,
	"description": typeString,
	"timeout":     typeString,
	"cwd":         typeString,
	"policy":      typeString,
	"params":      typeString,
}

// Limits on what a root's writable list and an alias may hold.
const (
	MaxWritable         = 64
	maxWritableLen      = 1024
	maxDescription      = 200
	maxAliasParams      = 32
	maxAliasParamValues = 32
)

// The aliases' policies: whether running one is allowed, asks, or is refused.
const (
	AliasAllow = "allow"
	AliasAsk   = "ask"
	AliasDeny  = "deny"
)

// Alias is an [alias.NAME] table: a command the host runs for a session,
// as argv with enum parameters, never through a shell.
type Alias struct {
	Name        string
	Description string
	// Command is the argv; Command[0] is absolute and clean. An element
	// that is exactly {KEY} for a key of Params is replaced by its value.
	Command []string
	// Timeout is how long it may run; 0 for no limit.
	Timeout time.Duration
	// Cwd is the host directory it runs in ("" for a fresh empty one),
	// with a ~ not yet expanded in a File's, expanded in a Config's.
	Cwd string
	// Policy is AliasAllow (the default), AliasAsk or AliasDeny.
	Policy string
	// Params are the enum parameters: name to the values it may take.
	Params map[string][]string
}

var (
	paramValueSyntax = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	cwdSyntax        = regexp.MustCompile(`^(~|~/.*|/.*)$`)
)

// printableText reports whether s is text a person can read where it is
// shown: valid UTF-8 with no control, format (bidi) or line-separator
// characters.
func printableText(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r) {
			return false
		}
	}
	return true
}

// CheckWritable checks a root's writable list: at most MaxWritable
// entries, each a path relative to the root, clean, with no .. component,
// no leading /, no control character and no : or , (docker's -v syntax),
// none twice and none inside another; "." (the whole root) is the only
// entry when it is there. Whether each exists, and is no symlink, is
// checked when the root is resolved (ResolveRoots).
func CheckWritable(list []string) error {
	if len(list) > MaxWritable {
		return fmt.Errorf("writable lists %d paths, at most %d", len(list), MaxWritable)
	}
	for _, e := range list {
		switch {
		case e == "":
			return errors.New(`writable has an empty path (use "." for the whole root)`)
		case len(e) > maxWritableLen:
			return fmt.Errorf("writable path of %d bytes is too long", len(e))
		case strings.HasPrefix(e, "/"):
			return fmt.Errorf("writable %q must be relative to the root, not absolute", e)
		case !printableText(e):
			return fmt.Errorf("writable %q has a control or invisible character", e)
		case strings.ContainsAny(e, ":,\\"):
			return fmt.Errorf(`writable %q has a ":", "," or "\\"`, e)
		case slices.Contains(strings.Split(e, "/"), ".."):
			return fmt.Errorf(`writable %q has a ".." component`, e)
		case path.Clean(e) != e:
			return fmt.Errorf("writable %q is not a clean path (%q is)", e, path.Clean(e))
		}
	}
	if len(list) > 1 && slices.Contains(list, ".") {
		return errors.New(`writable "." is the whole root and must be the only entry`)
	}
	for i, a := range list {
		for j, b := range list {
			switch {
			case i < j && a == b:
				return fmt.Errorf("writable lists %q twice", a)
			case i != j && a != b && strings.HasPrefix(b, a+"/"):
				return fmt.Errorf("writable %q is inside writable %q, which already covers it", b, a)
			}
		}
	}
	return nil
}

// readAliases reads the [alias.NAME] tables.
func (f *File) readAliases(v any) (map[string]Alias, error) {
	named, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("alias must be tables, [alias.NAME]")
	}
	aliases := map[string]Alias{}
	for _, name := range slices.Sorted(maps.Keys(named)) {
		t, ok := named[name].(map[string]any)
		if !ok {
			return nil, errors.New("alias must be tables, [alias.NAME]")
		}
		if !ValidProfileName(name) {
			return nil, fmt.Errorf("[alias.%s]: '%s' is not an alias name: lowercase letters, digits, - and _, starting with a letter or digit, at most 32", name, name)
		}
		table := aliasKey + "." + name
		a := Alias{Name: name, Policy: AliasAllow}
		for _, k := range slices.Sorted(maps.Keys(t)) {
			if _, ok := aliasKeys[k]; !ok {
				return nil, errors.New(unknownKey(table, k))
			}
			if k == "params" {
				continue
			}
			if err := f.readValue(table+"."+k, t[k], aliasKeys[k]); err != nil {
				return nil, err
			}
		}
		if v, ok := f.Vals[table+".command"].([]any); ok {
			for _, e := range v {
				a.Command = append(a.Command, e.(string))
			}
		}
		if len(a.Command) == 0 {
			return nil, fmt.Errorf("[alias.%s] needs command, an array: the program's absolute path, then its arguments", name)
		}
		if c := a.Command[0]; !path.IsAbs(c) || path.Clean(c) != c || c == "/" {
			return nil, fmt.Errorf("command in [alias.%s]: %q must be an absolute, clean path to a program", name, c)
		}
		for _, e := range a.Command {
			if !printableText(e) || strings.ContainsRune(e, 0) {
				return nil, fmt.Errorf("command in [alias.%s] has a control or invisible character", name)
			}
		}
		if d, ok := f.Vals[table+".description"].(string); ok {
			if len(d) > maxDescription || !printableText(d) {
				return nil, fmt.Errorf("description in [alias.%s] must be printable text of at most %d bytes", name, maxDescription)
			}
			a.Description = d
		}
		if d, ok := f.Vals[table+".timeout"].(string); ok {
			dur, err := time.ParseDuration(d)
			if err != nil || dur < 0 {
				return nil, fmt.Errorf(`timeout in [alias.%s]: %q is not a duration ("30s", "10m", "0" for none)`, name, d)
			}
			a.Timeout = dur
		}
		if c, ok := f.Vals[table+".cwd"].(string); ok {
			if c != "" && (!cwdSyntax.MatchString(c) || !printableText(c) || strings.ContainsRune(c, ':')) {
				return nil, fmt.Errorf(`cwd in [alias.%s]: %q must be an absolute path on this machine (or start with ~/)`, name, c)
			}
			a.Cwd = c
		}
		if p, ok := f.Vals[table+".policy"].(string); ok {
			switch p {
			case AliasAllow, AliasAsk, AliasDeny:
				a.Policy = p
			default:
				return nil, fmt.Errorf(`policy in [alias.%s]: %q is not "allow", "ask" or "deny"`, name, p)
			}
		}
		if pv, ok := t["params"]; ok {
			params, err := f.readParams(table, pv)
			if err != nil {
				return nil, err
			}
			a.Params = params
		}
		aliases[name] = a
	}
	return aliases, nil
}

// readParams reads an alias's params sub-table: names, each an array of
// 1 to 32 distinct values of [A-Za-z0-9._-]{1,64}.
func (f *File) readParams(table string, v any) (map[string][]string, error) {
	t, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("params in [%s] must be a table, [%s.params], of arrays of values", table, table)
	}
	if len(t) > maxAliasParams {
		return nil, fmt.Errorf("[%s.params] has %d parameters, at most %d", table, len(t), maxAliasParams)
	}
	params := map[string][]string{}
	for _, k := range slices.Sorted(maps.Keys(t)) {
		if !ValidProfileName(k) {
			return nil, fmt.Errorf("[%s.params]: '%s' is not a parameter name: lowercase letters, digits, - and _, starting with a letter or digit, at most 32", table, k)
		}
		list, ok := t[k].([]any)
		if !ok || len(list) < 1 || len(list) > maxAliasParamValues {
			return nil, fmt.Errorf("%s in [%s.params] must be an array of 1 to %d values", k, table, maxAliasParamValues)
		}
		if err := f.readValue(table+".params."+k, t[k], typeStrings); err != nil {
			return nil, err
		}
		var values []string
		for _, e := range list {
			s := e.(string)
			if !paramValueSyntax.MatchString(s) {
				return nil, fmt.Errorf("%s in [%s.params]: %q is not a value: letters, digits, . _ and -, 1 to 64", k, table, s)
			}
			if slices.Contains(values, s) {
				return nil, fmt.Errorf("%s in [%s.params] lists %q twice", k, table, s)
			}
			values = append(values, s)
		}
		params[k] = values
	}
	return params, nil
}

// CheckOrigin reports whether s is an origin the way open_urls_allow
// writes it: http or https, a lowercase host and an optional port, nothing
// else (no path, not even a "/", no credentials, query or fragment).
func CheckOrigin(s string) error {
	bad := fmt.Errorf(`%q is not an origin: http:// or https://, a lowercase host, an optional :port, nothing after it`, s)
	if !originSyntax.MatchString(s) {
		return bad
	}
	u, err := url.Parse(s)
	if err != nil || u.Hostname() == "" || strings.Contains(u.Hostname(), "..") || portTooBig(u.Port()) || s != u.Scheme+"://"+u.Host {
		return bad
	}
	return nil
}

var originSyntax = regexp.MustCompile(`^https?://([a-z0-9]([a-z0-9.-]*[a-z0-9])?|\[[0-9a-f:.]+\])(:[1-9][0-9]{0,4})?$`)

// checkPolicyKeys checks what the file says of the policy that needs more
// than a key's type.
func (f *File) checkPolicyKeys() error {
	if t, ok := f.Vals["trust"].(string); ok && t != TrustFallible && t != TrustHostile {
		return fmt.Errorf(`trust: %q is not "fallible" or "hostile"`, t)
	}
	if v, ok := f.Vals["link.open_urls_allow"].([]any); ok {
		for _, e := range v {
			if err := CheckOrigin(e.(string)); err != nil {
				return fmt.Errorf("open_urls_allow in [link]: %v", err)
			}
		}
	}
	return nil
}

func portTooBig(p string) bool {
	n := 0
	for _, c := range p {
		n = n*10 + int(c-'0')
	}
	return n > 65535
}
