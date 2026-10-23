package proposal

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/bfreis/caboose/internal/config"
)

// The bounds of a proposed alias.
const (
	// MaxAliasCommand is the most elements its command has, and
	// MaxAliasElement the most bytes in one.
	MaxAliasCommand = 64
	MaxAliasElement = 1024
	// maxAliasParams is the most parameters it has, and
	// maxAliasParamValues the most values one takes.
	maxAliasParams      = 32
	maxAliasParamValues = 32
	maxAliasDescription = 200
	maxAliasCwd         = 1024
)

var (
	paramValue = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	cwdSpelled = regexp.MustCompile(`^(~|~/.*|/.*)$`)
)

// Alias is an [alias.NAME] table to add to config.toml, or to replace one
// of that name with: the host command a session may then run with
// caboose-agent run. Absent keys are "": the alias takes config.toml's
// defaults for them (no description, no timeout, a fresh directory, policy
// "allow").
type Alias struct {
	Name        string
	Command     []string
	Description string
	// Timeout is as written: a Go duration, "0" for none.
	Timeout string
	// Cwd is as written: an absolute path, or one starting with ~/.
	Cwd string
	// Policy is "", "allow" or "ask": a proposal never makes an alias
	// that is refused, which would be no request at all.
	Policy string
	// Params are its enum parameters, name to values; nil for none.
	Params map[string][]string
}

// aliasFile is an [alias.NAME] table as written.
type aliasFile struct {
	Command     []string            `toml:"command"`
	Description *string             `toml:"description"`
	Timeout     *string             `toml:"timeout"`
	Cwd         *string             `toml:"cwd"`
	Policy      *string             `toml:"policy"`
	Params      map[string][]string `toml:"params"`
}

// checkAlias checks an [alias] table: one alias, by a name config.toml
// takes, with exactly config.toml's keys, every one within its bounds and
// what a terminal shows as it is. What it names on the host (its command,
// its directory) is the host's to judge, in caboose apply.
func checkAlias(m map[string]aliasFile) (*Alias, error) {
	if len(m) != 1 {
		return nil, errors.New("[alias] proposes more than one alias, or none: a proposal adds one at most, so that each is confirmed on its own")
	}
	var name string
	var f aliasFile
	for k, v := range m {
		name, f = k, v
	}
	if !config.ValidProfileName(name) {
		return nil, fmt.Errorf("'%s' is not an alias name: lowercase letters, digits, - and _, starting with a letter or digit, at most 32", Printable(clip(name, 40)))
	}
	t := "alias." + name
	a := &Alias{Name: name}

	if len(f.Command) == 0 {
		return nil, fmt.Errorf("[%s] needs command: the program's absolute path, then its arguments", t)
	}
	if len(f.Command) > MaxAliasCommand {
		return nil, fmt.Errorf("%s.command has %d elements, at most %d", t, len(f.Command), MaxAliasCommand)
	}
	for i, e := range f.Command {
		if e == "" {
			return nil, fmt.Errorf("%s.command[%d] is empty", t, i)
		}
		if len(e) > MaxAliasElement {
			return nil, fmt.Errorf("%s.command[%d] is over %d bytes", t, i, MaxAliasElement)
		}
		if strings.ContainsRune(e, '\t') {
			return nil, fmt.Errorf("%s.command[%d] has a tab", t, i)
		}
		if err := checkText(fmt.Sprintf("%s.command[%d]", t, i), e, false, MaxAliasElement); err != nil {
			return nil, err
		}
	}
	if c := f.Command[0]; !path.IsAbs(c) || path.Clean(c) != c || c == "/" {
		return nil, fmt.Errorf("%s.command[0], %q, must be the program's absolute, clean path", t, Printable(clip(c, 200)))
	}
	a.Command = slices.Clone(f.Command)

	if f.Description != nil {
		d := *f.Description
		if len(d) > maxAliasDescription {
			return nil, fmt.Errorf("%s.description is over %d bytes", t, maxAliasDescription)
		}
		if err := checkText(t+".description", d, false, maxAliasDescription); err != nil {
			return nil, err
		}
		a.Description = d
	}
	if f.Timeout != nil {
		d := *f.Timeout
		if err := checkText(t+".timeout", d, false, 32); err != nil {
			return nil, err
		}
		if dur, err := time.ParseDuration(d); err != nil || dur < 0 {
			return nil, fmt.Errorf(`%s.timeout, %q, is not a duration ("30s", "10m", "0" for none)`, t, Printable(clip(d, 40)))
		}
		a.Timeout = d
	}
	if f.Cwd != nil {
		c := *f.Cwd
		if len(c) > maxAliasCwd {
			return nil, fmt.Errorf("%s.cwd is over %d bytes", t, maxAliasCwd)
		}
		if err := checkText(t+".cwd", c, false, maxAliasCwd); err != nil {
			return nil, err
		}
		if c != "" && (!cwdSpelled.MatchString(c) || strings.ContainsRune(c, ':') || strings.ContainsRune(c, '\t')) {
			return nil, fmt.Errorf("%s.cwd, %q, must be an absolute path on the host, or one starting with ~/", t, Printable(clip(c, 200)))
		}
		if c != "" && c != "~" && path.Clean(c) != c {
			return nil, fmt.Errorf("%s.cwd, %q, is not a clean path", t, Printable(clip(c, 200)))
		}
		a.Cwd = c
	}
	if f.Policy != nil {
		switch p := *f.Policy; p {
		case config.AliasAllow, config.AliasAsk:
			a.Policy = p
		default:
			return nil, fmt.Errorf(`%s.policy, %q, is not "allow" or "ask"`, t, Printable(clip(p, 40)))
		}
	}
	if f.Params != nil {
		if len(f.Params) > maxAliasParams {
			return nil, fmt.Errorf("[%s.params] has %d parameters, at most %d", t, len(f.Params), maxAliasParams)
		}
		a.Params = map[string][]string{}
		for k, vs := range f.Params {
			if !config.ValidProfileName(k) {
				return nil, fmt.Errorf("[%s.params]: '%s' is not a parameter name: lowercase letters, digits, - and _, starting with a letter or digit, at most 32", t, Printable(clip(k, 40)))
			}
			if len(vs) < 1 || len(vs) > maxAliasParamValues {
				return nil, fmt.Errorf("%s.params.%s must list 1 to %d values", t, k, maxAliasParamValues)
			}
			for _, v := range vs {
				if !paramValue.MatchString(v) {
					return nil, fmt.Errorf("%s.params.%s: %q is not a value: letters, digits, . _ and -, 1 to 64", t, k, Printable(clip(v, 70)))
				}
			}
			for i, v := range vs {
				if slices.Contains(vs[:i], v) {
					return nil, fmt.Errorf("%s.params.%s lists %s twice", t, k, v)
				}
			}
			a.Params[k] = slices.Clone(vs)
		}
	}
	return a, nil
}

// Edit is what a sets in config.toml, by dotted key, as config.Edit's
// Set takes it: the keys it proposes, and no others.
func (a *Alias) Edit() map[string]any {
	t := "alias." + a.Name
	set := map[string]any{t + ".command": slices.Clone(a.Command)}
	for k, v := range map[string]string{"description": a.Description, "timeout": a.Timeout, "cwd": a.Cwd, "policy": a.Policy} {
		if v != "" {
			set[t+"."+k] = v
		}
	}
	for k, vs := range a.Params {
		set[t+".params."+k] = slices.Clone(vs)
	}
	return set
}
