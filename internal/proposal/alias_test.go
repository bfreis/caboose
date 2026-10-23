package proposal

import (
	"slices"
	"strings"
	"testing"

	"github.com/bfreis/caboose/internal/config"
)

const goodAlias = `title = "Run the tests on the host"
reason = "they need the host's GPU"

[alias.test]
command = ["/usr/local/bin/runtests", "--suite", "{suite}"]
description = "run the test suite"
timeout = "10m"
cwd = "~/src/proj"
policy = "ask"

[alias.test.params]
suite = ["unit", "all"]
`

func TestAliasProposal(t *testing.T) {
	p, err := Parse("test.toml", []byte(goodAlias))
	if err != nil {
		t.Fatal(err)
	}
	a := p.Alias
	if a == nil || a.Name != "test" || !slices.Equal(a.Command, []string{"/usr/local/bin/runtests", "--suite", "{suite}"}) ||
		a.Description != "run the test suite" || a.Timeout != "10m" || a.Cwd != "~/src/proj" || a.Policy != "ask" ||
		!slices.Equal(a.Params["suite"], []string{"unit", "all"}) {
		t.Fatalf("%+v", a)
	}
	e := a.Edit()
	for _, k := range []string{"alias.test.command", "alias.test.description", "alias.test.timeout", "alias.test.cwd", "alias.test.policy", "alias.test.params.suite"} {
		if _, ok := e[k]; !ok {
			t.Errorf("the edit sets no %s: %v", k, e)
		}
	}
	// What it sets, config.toml reads back as the same alias.
	data := config.EditFile(nil, config.Edit{Set: e})
	f, err := config.ParseFile("config.toml", data)
	if err != nil {
		t.Fatalf("%v\n%s", err, data)
	}
	got := f.Aliases["test"]
	if !slices.Equal(got.Command, a.Command) || got.Policy != "ask" || got.Cwd != "~/src/proj" || !slices.Equal(got.Params["suite"], a.Params["suite"]) {
		t.Errorf("read back %+v\n%s", got, data)
	}

	// The fewest keys: a command alone; the rest config.toml's defaults.
	p, err = Parse("awake.toml", []byte("title = \"t\"\n[alias.awake]\ncommand = [\"/usr/bin/caffeinate\", \"-i\"]\n"))
	if err != nil || p.Alias.Policy != "" || p.Alias.Cwd != "" || len(p.Alias.Edit()) != 1 {
		t.Fatalf("%+v %v", p, err)
	}
}

func TestAliasProposalRefusals(t *testing.T) {
	head := "title = \"t\"\n"
	cmd := "command = [\"/usr/bin/true\"]\n"
	long := strings.Repeat("a", MaxAliasElement+1)
	var many []string
	for range MaxAliasCommand + 1 {
		many = append(many, `"x"`)
	}
	var params strings.Builder
	for i := range 33 {
		params.WriteString("p" + string(rune('a'+i%26)) + string(rune('a'+i/26)) + " = [\"v\"]\n")
	}
	for _, c := range []struct{ data, want string }{
		{"[alias]\n", "more than one alias, or none"},
		{"[alias.a]\n" + cmd + "[alias.b]\n" + cmd, "more than one alias"},
		{"[alias.A]\n" + cmd, "not an alias name"},
		{"[alias.\"a b\"]\n" + cmd, "not an alias name"},
		{"[alias.x]\n", "needs command"},
		{"[alias.x]\ncommand = []\n", "needs command"},
		{"[alias.x]\ncommand = [\"true\"]\n", "absolute, clean path"},
		{"[alias.x]\ncommand = [\"/usr/bin/../bin/true\"]\n", "absolute, clean path"},
		{"[alias.x]\ncommand = [\"/\"]\n", "absolute, clean path"},
		{"[alias.x]\ncommand = [\"/usr/bin/true\", \"a\\u0000b\"]\n", "character"},
		{"[alias.x]\ncommand = [\"/usr/bin/true\", \"a\\u001b[2Jb\"]\n", "character"},
		{"[alias.x]\ncommand = [\"/usr/bin/true\", \"a\\u202eb\"]\n", "character"},
		{"[alias.x]\ncommand = [\"/usr/bin/true\", \"a\\nb\"]\n", "character"},
		{"[alias.x]\ncommand = [\"/usr/bin/true\", \"a\\tb\"]\n", "tab"},
		{"[alias.x]\ncommand = [\"/usr/bin/true\", \"\"]\n", "is empty"},
		{"[alias.x]\ncommand = [\"/usr/bin/true\", \"" + long + "\"]\n", "over 1024 bytes"},
		{"[alias.x]\ncommand = [\"/usr/bin/true\", " + strings.Join(many, ", ") + "]\n", "at most 64"},
		{"[alias.x]\ncommand = \"/usr/bin/true\"\n", "not a proposal"},
		{"[alias.x]\n" + cmd + "shell = true\n", "unknown key alias.x.shell"},
		{"[alias.x]\n" + cmd + "env = [\"A=b\"]\n", "unknown key alias.x.env"},
		{"[alias.x]\n" + cmd + "description = \"" + strings.Repeat("d", 201) + "\"\n", "over 200 bytes"},
		{"[alias.x]\n" + cmd + "description = \"a\\u001bb\"\n", "character"},
		{"[alias.x]\n" + cmd + "timeout = \"soon\"\n", "not a duration"},
		{"[alias.x]\n" + cmd + "timeout = \"-1s\"\n", "not a duration"},
		{"[alias.x]\n" + cmd + "cwd = \"relative\"\n", "absolute path"},
		{"[alias.x]\n" + cmd + "cwd = \"/a:b\"\n", "absolute path"},
		{"[alias.x]\n" + cmd + "cwd = \"/a/../b\"\n", "not a clean path"},
		{"[alias.x]\n" + cmd + "cwd = \"~user/x\"\n", "absolute path"},
		{"[alias.x]\n" + cmd + "policy = \"deny\"\n", `not "allow" or "ask"`},
		{"[alias.x]\n" + cmd + "policy = \"always\"\n", `not "allow" or "ask"`},
		{"[alias.x]\n" + cmd + "[alias.x.params]\nSuite = [\"a\"]\n", "not a parameter name"},
		{"[alias.x]\n" + cmd + "[alias.x.params]\nsuite = []\n", "1 to 32 values"},
		{"[alias.x]\n" + cmd + "[alias.x.params]\nsuite = [\"a b\"]\n", "not a value"},
		{"[alias.x]\n" + cmd + "[alias.x.params]\nsuite = [\"$(id)\"]\n", "not a value"},
		{"[alias.x]\n" + cmd + "[alias.x.params]\nsuite = [\"/etc\"]\n", "not a value"},
		{"[alias.x]\n" + cmd + "[alias.x.params]\nsuite = [\"a\", \"a\"]\n", "twice"},
		{"[alias.x]\n" + cmd + "[alias.x.params]\n" + params.String(), "at most 32"},
		{"[alias.x]\n" + cmd + "[alias.x.params]\nsuite = \"a\"\n", "not a proposal"},
		{"[alias.x]\n" + cmd + "[alias.x.params.deeper]\nsuite = [\"a\"]\n", "not a proposal"},
		{"[roots]\nx = \"/x\"\n[alias.x]\n" + cmd, "proposed alone"},
		{"[packages]\nadd = [\"jq\"]\n[alias.x]\n" + cmd, "proposed alone"},
	} {
		_, err := Parse("p.toml", []byte(head+c.data))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: %v, want %q", c.data, err, c.want)
		}
	}
}

// FuzzAliasProposal: whatever a proposal holds, Parse never panics, and
// an alias it takes is within every bound, and reads back from config.toml
// as itself.
func FuzzAliasProposal(f *testing.F) {
	f.Add([]byte(goodAlias))
	f.Add([]byte("title = \"t\"\n[alias.x]\ncommand = [\"/bin/x\", \"{a}\"]\n[alias.x.params]\na = [\"1\"]\n"))
	f.Add([]byte("title = \"t\"\n[alias.x]\ncommand = [\"/bin/x\"]\ncwd = \"~\"\ntimeout = \"0\"\n"))
	f.Add([]byte("title = \"t\"\n[alias.x]\ncommand = [\"/bin/x\", \"\\u202e\"]\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		p, err := Parse("p.toml", data)
		if err != nil || p.Alias == nil {
			return
		}
		a := p.Alias
		if !config.ValidProfileName(a.Name) || len(a.Command) == 0 || len(a.Command) > MaxAliasCommand || !strings.HasPrefix(a.Command[0], "/") {
			t.Fatalf("took %+v", a)
		}
		for _, e := range a.Command {
			if len(e) > MaxAliasElement || Printable(e) != e || strings.ContainsAny(e, "\t\n") {
				t.Fatalf("took the element %q", e)
			}
		}
		if a.Policy != "" && a.Policy != "allow" && a.Policy != "ask" {
			t.Fatalf("took the policy %q", a.Policy)
		}
		for k, vs := range a.Params {
			for _, v := range vs {
				if !paramValue.MatchString(v) || !config.ValidProfileName(k) {
					t.Fatalf("took the parameter %q = %q", k, v)
				}
			}
		}
		out := config.EditFile(nil, config.Edit{Set: a.Edit()})
		fl, err := config.ParseFile("config.toml", out)
		if err != nil {
			t.Fatalf("config.toml does not take what the proposal does: %v\n%s", err, out)
		}
		if got := fl.Aliases[a.Name]; !slices.Equal(got.Command, a.Command) {
			t.Fatalf("read back %q, proposed %q\n%s", got.Command, a.Command, out)
		}
	})
}
