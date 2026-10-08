package assets

import (
	"strings"
	"testing"
)

const fooBody = "ARG FOO_VERSION=2.3.0\nRUN curl -fsSL https://example.invalid/foo.tgz | tar -xz -C /usr/local/bin foo\n"

// A new section goes at the end, after a blank line, and the file reads
// back.
func TestSetSectionAppends(t *testing.T) {
	base, err := Seed(DefaultSections())
	if err != nil {
		t.Fatal(err)
	}
	out, err := SetSection(base, "foo", "foo 2.3", fooBody)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	want := "\n\n# caboose:section foo foo 2.3\n" + fooBody + "# caboose:end\n"
	if !strings.HasSuffix(s, want) {
		t.Errorf("does not end with the section:\n%s", s[len(s)-300:])
	}
	if !strings.HasPrefix(s, strings.TrimSuffix(string(base), "\n")) {
		t.Error("the rest of the Dockerfile changed")
	}
}

// A section of the same name is replaced where it is.
func TestSetSectionReplaces(t *testing.T) {
	in := "FROM x\n# caboose:section foo old\nRUN old\n# caboose:end\nRUN after\n"
	out, err := SetSection([]byte(in), "foo", "new", "RUN new\n")
	if err != nil {
		t.Fatal(err)
	}
	if want := "FROM x\n# caboose:section foo new\nRUN new\n# caboose:end\nRUN after\n"; string(out) != want {
		t.Errorf("got\n%s\nwant\n%s", out, want)
	}
	// An off section of that name is replaced by one that is on.
	in = "FROM x\n# caboose:section foo off old\n# RUN old\n# caboose:end\n"
	out, err = SetSection([]byte(in), "foo", "new", "RUN new")
	if err != nil {
		t.Fatal(err)
	}
	if want := "FROM x\n# caboose:section foo new\nRUN new\n# caboose:end\n"; string(out) != want {
		t.Errorf("got\n%s\nwant\n%s", out, want)
	}
}

func TestSetSectionRefuses(t *testing.T) {
	for _, tc := range []struct{ name, title, body, want string }{
		{"Foo", "t", "RUN x", "not a section name"},
		{"foo", "", "RUN x", "one line"},
		{"foo", "a\nb", "RUN x", "one line"},
		{"foo", "off the shelf", "RUN x", "off"},
		{"foo", "t", "  ", "empty"},
		{"foo", "t", "RUN x\n# caboose:end\nRUN y", "marker"},
		{"foo", "t", "RUN x\n  # caboose:section bar B", "marker"},
		{"foo", "t", "FROM evil/image", "FROM"},
		{"foo", "t", "RUN true \\\n from evil", "FROM"},
		{"foo", "t", "onbuild RUN x", "ONBUILD"},
		{"foo", "t", "RUN --network=host curl x", "--network"},
		{"foo", "t", "RUN --security=insecure x", "--security"},
	} {
		if _, err := SetSection([]byte("FROM x\n"), tc.name, tc.title, tc.body); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s %q %q: %v, want %q", tc.name, tc.title, tc.body, err, tc.want)
		}
	}
	// A Dockerfile whose own markers are broken is not edited.
	if _, err := SetSection([]byte("# caboose:section a A\n"), "foo", "t", "RUN x"); err == nil {
		t.Error("edited a Dockerfile with a section never ended")
	}
}
