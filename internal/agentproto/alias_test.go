package agentproto

import (
	"strings"
	"testing"
)

func TestParseAliasArgv(t *testing.T) {
	name, args, err := ParseAliasArgv([]string{"test", "suite=unit", "os=mac-14.2_x"})
	if err != nil || name != "test" || len(args) != 2 || args["suite"] != "unit" || args["os"] != "mac-14.2_x" {
		t.Fatalf("%q %v %v", name, args, err)
	}
	if _, args, err := ParseAliasArgv([]string{"awake"}); err != nil || len(args) != 0 {
		t.Fatalf("no arguments: %v %v", args, err)
	}
	many := []string{"x"}
	for range MaxAliasArgs + 1 {
		many = append(many, "k=v")
	}
	for _, argv := range [][]string{
		nil, {""}, {"Test"}, {"-x"}, {"a b"}, {strings.Repeat("a", 33)},
		{"t", "suite"}, {"t", "=unit"}, {"t", "suite="}, {"t", "Suite=unit"}, {"t", "suite=a b"},
		{"t", "suite=$(id)"}, {"t", "suite=/etc/passwd"}, {"t", "suite=a=b"}, {"t", "suite=unit", "suite=all"},
		{"t", "suite=" + strings.Repeat("a", 65)}, {"t", "s=\x1b[2J"}, many,
	} {
		if _, _, err := ParseAliasArgv(argv); err == nil {
			t.Errorf("%q: taken", argv)
		}
	}
}

// FuzzParseAliasArgv: what it takes is a name and KEY=VALUE pairs of the
// syntax config.toml's aliases take, and nothing else.
func FuzzParseAliasArgv(f *testing.F) {
	f.Add("test", "suite=unit", "os=mac")
	f.Add("t", "a=b=c", "")
	f.Add("x", "k=\u202e", "k=v")
	f.Fuzz(func(t *testing.T, a, b, c string) {
		name, args, err := ParseAliasArgv([]string{a, b, c})
		if err != nil {
			return
		}
		if !aliasName.MatchString(name) || len(args) != 2 {
			t.Fatalf("took %q %v", name, args)
		}
		for k, v := range args {
			if !aliasName.MatchString(k) || !aliasValue.MatchString(v) || strings.ContainsAny(v, "/=$ ;") {
				t.Fatalf("took %q=%q", k, v)
			}
		}
	})
}
