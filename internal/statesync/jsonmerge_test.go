package statesync

import (
	"reflect"
	"testing"
)

func TestMergeJSON(t *testing.T) {
	for _, tc := range []struct {
		name               string
		base, ours, theirs string
		nilBase            bool
		prefer             Side
		want               string
		conflicts          []string
	}{
		{name: "disjoint keys",
			base: `{"a":1}`, ours: `{"a":1,"b":2}`, theirs: `{"a":1,"c":3}`,
			want: `{"a":1,"b":2,"c":3}`},
		{name: "one side changes, one side deletes, different keys",
			base: `{"a":1,"b":2}`, ours: `{"a":9,"b":2}`, theirs: `{"a":1}`,
			want: `{"a":9}`},
		{name: "same change on both sides",
			base: `{"a":1}`, ours: `{"a":2}`, theirs: `{"a":2}`,
			want: `{"a":2}`},
		{name: "nested objects merge",
			base: `{"env":{"A":"1"}}`, ours: `{"env":{"A":"1","B":"2"}}`, theirs: `{"env":{"A":"1","C":"3"}}`,
			want: `{"env":{"A":"1","B":"2","C":"3"}}`},
		{name: "both change one key",
			base: `{"theme":"dark","x":1}`, ours: `{"theme":"light","x":1}`, theirs: `{"theme":"solar","x":2}`,
			conflicts: []string{"theme"}},
		{name: "change against delete",
			base: `{"a":{"b":1}}`, ours: `{"a":{"b":2}}`, theirs: `{"a":{}}`,
			conflicts: []string{"a.b"}},
		{name: "arrays are values",
			base: `{"allow":["x"]}`, ours: `{"allow":["x","y"]}`, theirs: `{"allow":[]}`,
			conflicts: []string{"allow"}},
		{name: "prefer ours settles only the conflict",
			base: `{"t":"d","x":1}`, ours: `{"t":"l","x":1}`, theirs: `{"t":"s","x":2}`, prefer: Ours,
			want: `{"t":"l","x":2}`},
		{name: "prefer theirs",
			base: `{"t":"d"}`, ours: `{"t":"l"}`, theirs: `{"t":"s"}`, prefer: Theirs,
			want: `{"t":"s"}`},
		{name: "no base: both created the file",
			nilBase: true, ours: `{"a":1,"b":1}`, theirs: `{"a":1,"c":1}`,
			want: `{"a":1,"b":1,"c":1}`},
		{name: "no base, same key differently",
			nilBase: true, ours: `{"a":1}`, theirs: `{"a":2}`,
			conflicts: []string{"a"}},
		{name: "big numbers survive",
			base: `{}`, ours: `{"n":12345678901234567890}`, theirs: `{}`,
			want: `{"n":12345678901234567890}`},
		{name: "whole document changes type",
			base: `{}`, ours: `[]`, theirs: `{"a":1}`,
			conflicts: []string{"(the whole document)"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var base []byte
			if !tc.nilBase {
				base = []byte(tc.base)
			}
			got, conflicts, err := MergeJSON(base, []byte(tc.ours), []byte(tc.theirs), tc.prefer)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(conflicts, tc.conflicts) {
				t.Fatalf("conflicts = %v, want %v", conflicts, tc.conflicts)
			}
			if tc.conflicts != nil {
				return
			}
			want, err := decode([]byte(tc.want))
			if err != nil {
				t.Fatal(err)
			}
			wantOut, _ := Encode(want)
			if string(got) != string(wantOut) {
				t.Errorf("merged =\n%s\nwant\n%s", got, wantOut)
			}
		})
	}
}

func TestMergeJSONRefusesInvalid(t *testing.T) {
	if _, _, err := MergeJSON([]byte(`{}`), []byte(`{`), []byte(`{}`), NoSide); err == nil {
		t.Error("merged invalid JSON")
	}
	if _, _, err := MergeJSON([]byte(`{}`), []byte(`{} {}`), []byte(`{}`), NoSide); err == nil {
		t.Error("merged two documents")
	}
}

func TestEncodeKeepsHTML(t *testing.T) {
	out, err := Encode(map[string]any{"cmd": "a && b <x>"})
	if err != nil {
		t.Fatal(err)
	}
	if want := "{\n  \"cmd\": \"a && b <x>\"\n}\n"; string(out) != want {
		t.Errorf("Encode = %q, want %q", out, want)
	}
}
