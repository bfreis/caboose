package proposal

import (
	"strings"
	"testing"
)

func TestWritableProposal(t *testing.T) {
	for _, path := range []string{"/work/dev/proj", "/work", "/work/a b/c"} {
		p, err := Parse("w.toml", []byte("title = \"Write proj\"\n[writable]\npath = \""+path+"\"\n"))
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if p.Writable == nil || p.Writable.Path != path || p.Root != nil || p.Alias != nil {
			t.Errorf("%s: %+v", path, p)
		}
	}
}

func TestWritableProposalRefusals(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"empty", "[writable]\npath = \"\"\n", "needs path"},
		{"no path", "[writable]\n", "needs path"},
		{"relative", "[writable]\npath = \"work/dev\"\n", "absolute, clean"},
		{"dotdot", "[writable]\npath = \"/work/dev/../../etc\"\n", "absolute, clean"},
		{"trailing slash", "[writable]\npath = \"/work/dev/\"\n", "absolute, clean"},
		{"double slash", "[writable]\npath = \"/work//dev\"\n", "absolute, clean"},
		{"dot", "[writable]\npath = \"/work/./dev\"\n", "absolute, clean"},
		{"outside work", "[writable]\npath = \"/home/agent\"\n", "not under /work"},
		{"work prefix", "[writable]\npath = \"/workshop\"\n", "not under /work"},
		{"escape", "[writable]\npath = \"/work/dev/\\u001b[2Jx\"\n", "character U+001B"},
		{"bidi", "[writable]\npath = \"/work/dev/\\u202ex\"\n", "character U+202E"},
		{"newline", "[writable]\npath = \"/work/dev/a\\nb\"\n", "character U+000A"},
		{"colon", "[writable]\npath = \"/work/dev/a:b\"\n", "mount cannot carry"},
		{"too long", "[writable]\npath = \"/work/" + strings.Repeat("a", MaxWritablePath) + "\"\n", "over 1024 bytes"},
		{"unknown key", "[writable]\npath = \"/work/dev\"\nmode = \"rw\"\n", "unknown key writable.mode"},
		{"with a root", "[writable]\npath = \"/work/dev/p\"\n[roots]\nx = \"~/x\"\n", "proposed alone"},
		{"with a section", "dockerfile_sha256 = \"ab\"\n[writable]\npath = \"/work/dev/p\"\n[section]\nname = \"foo\"\ntitle = \"foo\"\nbody = \"RUN true\\n\"\n", "proposed alone"},
		{"with packages", "[writable]\npath = \"/work/dev/p\"\n[packages]\nadd = [\"jq\"]\n", "proposed alone"},
		{"with an alias", "[writable]\npath = \"/work/dev/p\"\n[alias.t]\ncommand = [\"/bin/true\"]\n", "alone"},
		{"two paths", "[writable]\npath = [\"/work/a\", \"/work/b\"]\n", "not a proposal"},
	} {
		_, err := Parse("w.toml", []byte("title = \"t\"\n"+tc.body))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", tc.name, err, tc.want)
		}
	}
}
