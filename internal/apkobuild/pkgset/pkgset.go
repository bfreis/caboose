// Package pkgset is caboose's package groups and the checks on package
// names: what an environment's apko image is made of, apart from apko
// itself, so that the configuration can check a package list without
// linking apko into everything that reads it (the sandbox's agent too).
// internal/apkobuild resolves and builds what it describes.
package pkgset

import (
	_ "embed"
	"fmt"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"
)

//go:embed packages.toml
var packagesTOML string

// Group is a named set of packages the sandbox offers together.
type Group struct {
	Name     string   `toml:"name"`
	Title    string   `toml:"title"`
	Packages []string `toml:"packages"`
	// Required marks the group the sandbox cannot work without.
	Required bool `toml:"required"`
	// Default marks a group installed unless the environment leaves it out.
	Default bool `toml:"default"`
}

var groups = mustGroups()

func mustGroups() []Group {
	var f struct {
		Group []Group `toml:"group"`
	}
	if _, err := toml.Decode(packagesTOML, &f); err != nil {
		panic("pkgset: packages.toml: " + err.Error())
	}
	return f.Group
}

// Groups returns the package groups in the order they are offered.
func Groups() []Group {
	out := make([]Group, len(groups))
	for i, g := range groups {
		g.Packages = slices.Clone(g.Packages)
		out[i] = g
	}
	return out
}

// Required returns the packages of the required group.
func Required() []string {
	var out []string
	for _, g := range groups {
		if g.Required {
			out = append(out, g.Packages...)
		}
	}
	return out
}

// DefaultPackages returns the packages of the required group and of every
// default group.
func DefaultPackages() []string {
	var out []string
	for _, g := range groups {
		if g.Required || g.Default {
			out = append(out, g.Packages...)
		}
	}
	return out
}

// CheckName reports why s is not an apk package name: non-empty, at most 128
// bytes, made of lowercase letters, digits and . _ + -, starting with a letter
// or a digit.
func CheckName(s string) error {
	if s == "" {
		return fmt.Errorf("package name is empty")
	}
	if len(s) > 128 {
		return fmt.Errorf("package name %q is longer than 128 bytes", truncate(s))
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case strings.IndexByte("._+-", c) >= 0:
			if i == 0 {
				return fmt.Errorf("package name %q must start with a letter or a digit", s)
			}
		default:
			return fmt.Errorf("package name %q has the character %q; only lowercase letters, digits and . _ + - are allowed", truncate(s), c)
		}
	}
	return nil
}

func truncate(s string) string {
	if len(s) > 40 {
		return s[:40] + "..."
	}
	return s
}

// Spec is what an environment asks for: its own packages, and whether the
// default groups come with them. The required group is always included.
type Spec struct {
	Packages []string
	Defaults bool
}

// Normal is the spec with its packages sorted and without duplicates, never
// nil: two specs that ask for the same thing are equal once normal.
func (s Spec) Normal() Spec {
	pkgs := slices.Clone(s.Packages)
	slices.Sort(pkgs)
	return Spec{Packages: append([]string{}, slices.Compact(pkgs)...), Defaults: s.Defaults}
}

// Equal reports whether s and o ask for the same thing: the same defaults,
// and the same packages in any order.
func (s Spec) Equal(o Spec) bool {
	a, b := s.Normal(), o.Normal()
	return a.Defaults == b.Defaults && slices.Equal(a.Packages, b.Packages)
}

// List returns the packages the spec stands for, checked, without duplicates
// and sorted.
func (s Spec) List() ([]string, error) {
	all := Required()
	if s.Defaults {
		all = append(all, DefaultPackages()...)
	}
	all = append(all, s.Packages...)
	for _, p := range all {
		if err := CheckName(p); err != nil {
			return nil, err
		}
	}
	slices.Sort(all)
	return slices.Compact(all), nil
}
