package main

import (
	"strings"
	"testing"

	"github.com/bfreis/caboose/internal/agent"
)

// A test outside internal/agent has no TestMain moving the link socket, so
// agent.Main must refuse before it does anything. An unknown command keeps
// this test from dialing anything even if the refusal were gone: it would
// print the usage instead.
func TestMainRefusesTheRealSocketInTests(t *testing.T) {
	var out, errb strings.Builder
	code := agent.Main([]string{"no-such-command"}, strings.NewReader(""), nopCloser{&out}, &errb)
	if code != 1 || !strings.Contains(errb.String(), "refused in a test binary") {
		t.Fatalf("exit %d, stderr %q: want the test-binary refusal", code, errb.String())
	}
}

type nopCloser struct{ *strings.Builder }

func (nopCloser) Close() error { return nil }
