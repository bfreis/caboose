package session

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// The same series tests/run.sh drives against the bash first_free_session:
// busy names are stepped over, orphaned ones reused.
func TestFirstFreeSeries(t *testing.T) {
	busy := func(names ...string) ClientCounter {
		return func(n string) int {
			for _, b := range names {
				if b == n {
					return 1
				}
			}
			return 0
		}
	}
	var got []string
	for _, c := range []ClientCounter{busy(), busy("proj"), busy("proj", "proj-2"), busy("proj-2")} {
		name, err := FirstFree("proj", c)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, name)
	}
	if want := "proj proj-2 proj-3 proj"; strings.Join(got, " ") != want {
		t.Errorf("series = %q, want %q", strings.Join(got, " "), want)
	}
}

// Every name up to the cap is taken: it gives up, rather than loop.
func TestFirstFreeGivesUp(t *testing.T) {
	var asked []string
	_, err := FirstFree("proj", func(n string) int { asked = append(asked, n); return 2 })
	if !errors.Is(err, ErrTooMany) {
		t.Fatalf("err = %v", err)
	}
	if len(asked) != MaxSeries || asked[MaxSeries-1] != "proj-64" {
		t.Errorf("asked %d names, last %q", len(asked), asked[len(asked)-1])
	}
	// The 64th is still a valid answer.
	name, err := FirstFree("proj", func(n string) int {
		if n == "proj-64" {
			return 0
		}
		return 1
	})
	if err != nil || name != "proj-64" {
		t.Errorf("name=%q err=%v", name, err)
	}
}

func digest(p string) string {
	s := sha256.Sum256([]byte(p))
	return hex.EncodeToString(s[:])[:6]
}

func TestNameFor(t *testing.T) {
	p := "/work/you/caboose"
	if got := NameFor(p, ""); got != "caboose-"+digest(p) {
		t.Errorf("NameFor = %q", got)
	}
	if got := NameFor(p, "two"); got != "caboose-"+digest(p)+"-two" {
		t.Errorf("NameFor with suffix = %q", got)
	}
	if NameFor("/a/caboose", "") == NameFor("/b/caboose", "") {
		t.Error("two repos with one basename share a session")
	}
	if got := NameFor("/a/my.repo:x", "a b.c"); got != "my-repo-x-"+digest("/a/my.repo:x")+"-a-b-c" {
		t.Errorf("sanitized = %q", got)
	}
}

func TestCountLines(t *testing.T) {
	for in, want := range map[string]int{"": 0, "/dev/pts/1: s [80x24]\n": 1, "a\nb\n": 2} {
		if got := CountLines(in); got != want {
			t.Errorf("CountLines(%q) = %d", in, got)
		}
	}
}

func TestProjectSessions(t *testing.T) {
	list := "proj\r\nproj-2\nproj-x\nproj-10\nother\nproj-2-two\nxproj\n"
	got := ProjectSessions(list, "proj")
	if want := []string{"proj", "proj-2", "proj-10"}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %q", got)
	}
}
