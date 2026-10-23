package checkpoint

import (
	"bytes"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func FuzzParseID(f *testing.F) {
	for _, s := range []string{
		"caboose-3fa1c2d0@20261017T231005Z", "claude-9b0e11aa@latest", "..@latest",
		"a/b-12345678@20261017T231005Z", "../x", "../x-12345678@latest", ".-12345678@latest",
		"x-12345678@../..", "x-12345678@20261017T231005Z\x00", "x-12345678@2026101T231005Z",
		"\\-12345678@latest", "x-12345678@@latest", "",
	} {
		f.Add(s)
	}
	store := "/store/checkpoints"
	f.Fuzz(func(t *testing.T, s string) {
		id, err := ParseID(s)
		if err != nil {
			return
		}
		k := id.Key
		if k == "" || k == "." || k == ".." || strings.ContainsAny(k, "/\\\x00") {
			t.Fatalf("ParseID(%q) gave key %q", s, k)
		}
		if id.String() != s {
			t.Fatalf("ParseID(%q).String() = %q", s, id.String())
		}
		p := filepath.Join(store, k)
		want := 1
		if !id.Latest {
			p = filepath.Join(p, FormatStamp(id.Stamp))
			want = 2
		}
		rel, err := filepath.Rel(store, p)
		if err != nil || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) || rel == "." {
			t.Fatalf("ParseID(%q): %q is outside the store", s, p)
		}
		if n := len(strings.Split(rel, string(filepath.Separator))); n != want {
			t.Fatalf("ParseID(%q): %q has %d components", s, rel, n)
		}
	})
}

func FuzzDecodeMeta(f *testing.F) {
	f.Add([]byte(`{"version":1,"id":"a-12345678@20261017T231005Z","entries":3}`))
	f.Add([]byte(`{"version":2}`))
	f.Add([]byte(`{"version":0}`))
	f.Add([]byte(`{"version":1,"shrunk":{"id":"x","entries":1}}`))
	f.Add([]byte(`[`))
	f.Add(bytes.Repeat([]byte(" "), maxMetaSize+1))
	f.Fuzz(func(t *testing.T, b []byte) {
		m, err := DecodeMeta(bytes.NewReader(b))
		if len(b) > maxMetaSize && err == nil {
			t.Fatal("over the bound accepted")
		}
		if err == nil && (m == nil || m.Version != MetaVersion) {
			t.Fatalf("accepted version %v", m)
		}
		if errors.Is(err, ErrNewerVersion) && m != nil {
			t.Fatal("meta with a newer version")
		}
	})
}
