package launcher

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/docker"
)

// mountsFake is a docker whose running container box has the mounts that
// mounts returns for the data dir, each "destination<TAB>source", as
// `docker inspect` lists them.
func mountsFake(t *testing.T, persist []config.Persist, mounts func(data string) []string) (*App, *bytes.Buffer) {
	t.Helper()
	tmp := t.TempDir()
	data := filepath.Join(tmp, "data")
	var lines string
	for _, m := range mounts(data) {
		lines += m + "\n"
	}
	if err := os.WriteFile(filepath.Join(tmp, "mounts"), []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
case "$*" in
  "inspect --type=container -f {{.State.Status}} box") echo running ;;
  "inspect --type=container box --format "*) cat "` + tmp + `/mounts" ;;
esac
exit 0
`
	fake := filepath.Join(tmp, "docker")
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	var errb bytes.Buffer
	return &App{
		Cfg: &config.Config{Container: "box", Image: "img", DataDir: data, Persist: persist,
			Getenv: func(string) string { return "" }},
		Docker: &docker.CLI{Path: fake},
		Stdout: &bytes.Buffer{}, Stderr: &errb,
	}, &errb
}

func TestPersistDrift(t *testing.T) {
	aws := mustPersist(t, "aws", "~/.aws")
	foo := mustPersist(t, "foo", "~/.config/foo")
	mounted := func(extra ...string) func(string) []string {
		return func(data string) []string {
			ms := []string{"/work\t/h/dev", "/home/agent/.ssh\t" + data + "/dot_ssh"}
			for _, e := range extra {
				ms = append(ms, strings.ReplaceAll(e, "DATA", data))
			}
			return ms
		}
	}
	for _, tc := range []struct {
		name    string
		persist []config.Persist
		mounts  func(string) []string
		want    string
	}{
		{"in step", []config.Persist{aws, foo},
			mounted("/home/agent/.aws\tDATA/persist/aws", "/home/agent/.config/foo\tDATA/persist/foo"), ""},
		{"an entry added since", []config.Persist{aws, foo},
			mounted("/home/agent/.aws\tDATA/persist/aws"),
			"persists ~/.aws (aws); the configuration says ~/.aws (aws), ~/.config/foo (foo)"},
		{"an entry moved since", []config.Persist{aws},
			mounted("/home/agent/.config/aws\tDATA/persist/aws"),
			"persists ~/.config/aws (aws); the configuration says ~/.aws (aws)"},
		{"an entry removed since", nil,
			mounted("/home/agent/.aws\tDATA/persist/aws"),
			"persists ~/.aws (aws); the configuration says none"},
		{"a mount from outside the data dir is no entry", nil,
			mounted("/home/agent/.aws\t/somewhere/persist/aws"), ""},
		{"no container: nothing to say", []config.Persist{aws},
			func(string) []string { return nil }, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, errb := mountsFake(t, tc.persist, tc.mounts)
			if d := a.persistDrift(); d != tc.want {
				t.Errorf("drift %q\nwant  %q", d, tc.want)
			}
			a.warnIfPersistDrifted()
			switch {
			case tc.want == "" && errb.Len() != 0:
				t.Errorf("warned with nothing to say: %q", errb.String())
			case tc.want != "" && !strings.Contains(errb.String(), "the container "+tc.want+".\ncaboose: run 'caboose restart'"):
				t.Errorf("warning %q", errb.String())
			}
		})
	}
}

// Status lists the entries, and where each is kept.
func TestStatusListsPersist(t *testing.T) {
	a, data, _ := runningFake(t, "dot_local/linux-arm64")
	a.Cfg.Persist = []config.Persist{mustPersist(t, "aws", "~/.aws")}
	if err := a.Status(); err != nil {
		t.Fatal(err)
	}
	out := a.Stdout.(*bytes.Buffer).String()
	if want := "persist   : ~/.aws -> " + data + "/persist/aws\n"; !strings.Contains(out, want) {
		t.Errorf("status lacks %q:\n%s", want, out)
	}
}
