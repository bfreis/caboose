package checkpoint

import (
	"math/rand"
	"strings"
	"testing"
	"time"
)

func TestKey(t *testing.T) {
	cases := map[string]string{
		"~/.claude":                     "claude-",
		"/work/dev/org/proj":            "proj-",
		"/work/dev/My Project!":         "my-project--",
		"/work/dev/x/":                  "x-",
		"":                              "path-",
		"/":                             "path-",
		"~/...":                         "path-",
		"/a/" + strings.Repeat("Z", 40): strings.Repeat("z", 32) + "-",
		"/a/é":                          "---",
	}
	for p, prefix := range cases {
		k := Key(p)
		if !strings.HasPrefix(k, prefix) || len(k) != len(prefix)+8 {
			t.Errorf("Key(%q) = %q, want %q + 8 hex", p, k, prefix)
		}
		if !ValidKey(k) {
			t.Errorf("Key(%q) = %q is not valid", p, k)
		}
	}
	if Key("/a/caboose") == Key("/b/caboose") {
		t.Error("two paths share a key")
	}
}

func TestKeyParsesProperty(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	stamp := FormatStamp(time.Date(2026, 10, 17, 23, 10, 5, 0, time.UTC))
	for i := 0; i < 20000; i++ {
		b := make([]byte, r.Intn(80))
		for j := range b {
			b[j] = byte(r.Intn(256))
		}
		p := string(b)
		if i%3 == 0 {
			p = "/work/" + p
		}
		id, err := ParseID(Key(p) + "@" + stamp)
		if err != nil {
			t.Fatalf("Key(%q) = %q: %v", p, Key(p), err)
		}
		if id.Key != Key(p) || FormatStamp(id.Stamp) != stamp {
			t.Fatalf("round trip of %q", p)
		}
	}
}

func TestParseStamp(t *testing.T) {
	good := time.Date(2026, 10, 17, 23, 10, 5, 0, time.UTC)
	s := FormatStamp(good.In(time.FixedZone("x", 3600)))
	if s != "20261017T231005Z" {
		t.Fatalf("FormatStamp = %q", s)
	}
	got, err := ParseStamp(s)
	if err != nil || !got.Equal(good) {
		t.Fatalf("ParseStamp(%q) = %v, %v", s, got, err)
	}
	for _, bad := range []string{
		"", "20261017T231005", "20261017T231005z", "20261017 231005Z", "2026-10-17T23:10:05Z",
		"20261317T231005Z", "20261032T231005Z", "20260230T231005Z", "20261017T241005Z",
		"20261017T236005Z", "20261017T231060Z", "20261017T231005Z ", "+0261017T231005Z",
		"2026101７T231005Z", "20261017T231005ZZ", "-0261017T231005Z",
	} {
		if _, err := ParseStamp(bad); err == nil {
			t.Errorf("ParseStamp(%q) accepted", bad)
		}
	}
}

func TestParseID(t *testing.T) {
	k := Key("/work/dev/org/proj")
	for _, good := range []string{k + "@20261017T231005Z", k + "@latest", "a-0123abcd@latest", "_-0123abcd@latest", "--0123abcd@latest"} {
		id, err := ParseID(good)
		if err != nil {
			t.Errorf("ParseID(%q): %v", good, err)
			continue
		}
		if id.String() != good {
			t.Errorf("String() = %q, want %q", id.String(), good)
		}
	}
	for _, bad := range []string{
		"", "@latest", k, k + "@", k + "@Latest", k + "@latest ", " " + k + "@latest",
		k + "@20261017T231005Z@latest", "..@latest", ".-0123abcd@latest", "..-0123abcd@latest",
		".x-0123abcd@latest", "a/b-12345678@latest", "../x-12345678@latest", "a\\b-12345678@latest",
		"a\x00-12345678@latest", "A-12345678@latest", "a-1234567@latest", "a-123456789@latest",
		"a-1234567G@latest", "a-ABCDEF12@latest", "é-12345678@latest",
		strings.Repeat("a", 33) + "-12345678@latest", k + "@20261017T231005Z/..",
	} {
		if _, err := ParseID(bad); err == nil {
			t.Errorf("ParseID(%q) accepted", bad)
		}
	}
}
