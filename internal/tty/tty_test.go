package tty

import (
	"os"
	"testing"
)

func TestRegularFileIsNotATerminal(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "x")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if IsTerminal(f.Fd()) {
		t.Error("a regular file reads as a terminal")
	}
}
