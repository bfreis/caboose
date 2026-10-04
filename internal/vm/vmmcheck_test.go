package vm

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// A check reads back as it was written, a value on one line whatever it
// held, and a key of a newer caboose-vmm's is skipped.
func TestCheckRoundTrip(t *testing.T) {
	c := Check{Version: "v1.2.3", OS: "darwin", Arch: "arm64", MacOS: "15.1", Translated: No, Framework: "ok",
		Supported: Yes, Entitled: No, Valid: "Invalid virtual machine configuration.\nThe process lacks it (VZErrorDomain 2)"}
	var b bytes.Buffer
	if err := c.Write(&b); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(b.String(), CheckFormat+"\n") || strings.Contains(b.String(), "error=") {
		t.Errorf("wrote %q", b.String())
	}
	got, err := ParseCheck(append(b.Bytes(), "later=key\n"...))
	if err != nil {
		t.Fatal(err)
	}
	want := c
	want.Valid = "Invalid virtual machine configuration. The process lacks it (VZErrorDomain 2)"
	if got != want {
		t.Errorf("read %+v\nwant %+v", got, want)
	}
	if got.Usable() {
		t.Error("usable without the entitlement")
	}
	want.Entitled = Yes
	if !want.Usable() {
		t.Error("not usable with everything")
	}
}

func TestParseCheckRefuses(t *testing.T) {
	for _, out := range []string{"", "usage: caboose-vmm DIR\n", "version=v1\n"} {
		if _, err := ParseCheck([]byte(out)); !errors.Is(err, ErrNoCheck) {
			t.Errorf("%q: %v", out, err)
		}
	}
	if _, err := ParseCheck(bytes.Repeat([]byte("x"), maxCheck+1)); err == nil {
		t.Error("an endless check read")
	}
}
