package statesync

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
)

// Side picks which side wins a key both sides changed differently.
type Side int

const (
	// NoSide leaves such keys as conflicts.
	NoSide Side = iota
	// Ours keeps this machine's value.
	Ours
	// Theirs takes the remote's value.
	Theirs
)

// absent marks a key missing from one side, so a deletion is a change like
// any other.
type absentT struct{}

var absent = absentT{}

// MergeJSON merges two JSON documents changed from a common base, key by
// key and recursively through objects: a key only one side changed takes
// that side's value (a deleted key included), and a key both sides changed
// to the same value takes it. Anything else -- both changed one key
// differently, or one side changed the document's shape -- is a conflict:
// its dotted path is returned, unless prefer names a side to take.
//
// A nil base is an empty object, as when both machines created the file
// before their first sync. Arrays are values, not merged element by
// element: a permission list edited on both sides is a conflict, which is
// better than a union that silently resurrects an entry one side removed.
//
// The result is indented two spaces, keys sorted, with no HTML escaping.
func MergeJSON(base, ours, theirs []byte, prefer Side) ([]byte, []string, error) {
	var b, o, t any = map[string]any{}, nil, nil
	if base != nil {
		v, err := decode(base)
		if err != nil {
			return nil, nil, fmt.Errorf("base: %w", err)
		}
		b = v
	}
	o, err := decode(ours)
	if err != nil {
		return nil, nil, fmt.Errorf("ours: %w", err)
	}
	t, err = decode(theirs)
	if err != nil {
		return nil, nil, fmt.Errorf("theirs: %w", err)
	}
	var conflicts []string
	merged := merge3(b, o, t, "", prefer, &conflicts)
	if len(conflicts) > 0 {
		return nil, conflicts, nil
	}
	out, err := Encode(merged)
	return out, nil, err
}

func merge3(b, o, t any, at string, prefer Side, conflicts *[]string) any {
	switch {
	case reflect.DeepEqual(o, t):
		return o
	case reflect.DeepEqual(b, o):
		return t
	case reflect.DeepEqual(b, t):
		return o
	}
	om, oOK := o.(map[string]any)
	tm, tOK := t.(map[string]any)
	if oOK && tOK {
		bm, ok := b.(map[string]any)
		if !ok {
			bm = map[string]any{}
		}
		out := map[string]any{}
		for _, k := range unionKeys(bm, om, tm) {
			v := merge3(get(bm, k), get(om, k), get(tm, k), join(at, k), prefer, conflicts)
			if v != absent {
				out[k] = v
			}
		}
		return out
	}
	switch prefer {
	case Ours:
		return o
	case Theirs:
		return t
	}
	*conflicts = append(*conflicts, or(at, "(the whole document)"))
	return o
}

func get(m map[string]any, k string) any {
	if v, ok := m[k]; ok {
		return v
	}
	return absent
}

func unionKeys(ms ...map[string]any) []string {
	seen := map[string]bool{}
	var keys []string
	for _, m := range ms {
		for k := range m {
			if !seen[k] {
				seen[k] = true
				keys = append(keys, k)
			}
		}
	}
	sort.Strings(keys)
	return keys
}

func join(at, k string) string {
	if at == "" {
		return k
	}
	return at + "." + k
}

func or(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// decode parses one JSON document, keeping numbers as written.
func decode(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, fmt.Errorf("trailing data after the JSON document")
	}
	return v, nil
}

// Encode writes v as MergeJSON does: two-space indent, sorted keys, no HTML
// escaping, a trailing newline.
func Encode(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ValidJSON reports whether data is exactly one JSON document.
func ValidJSON(data []byte) error {
	_, err := decode(data)
	return err
}
