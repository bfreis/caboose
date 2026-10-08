package launcher

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/bfreis/caboose/internal/assets"
	"github.com/bfreis/caboose/internal/backend"
	"github.com/bfreis/caboose/internal/backend/backendtest"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/datadir"
	"github.com/bfreis/caboose/internal/sandboxcfg"
	"github.com/bfreis/caboose/internal/version"
)

func TestHostTimezone(t *testing.T) {
	noLink := func(string) (string, error) { return "", errors.New("not a link") }
	link := func(s string) func(string) (string, error) {
		return func(string) (string, error) { return s, nil }
	}
	noFile := func(string) ([]byte, error) { return nil, os.ErrNotExist }
	file := func(s string) func(string) ([]byte, error) {
		return func(string) ([]byte, error) { return []byte(s), nil }
	}
	cases := []struct {
		name, ctz, tz string
		rl            func(string) (string, error)
		rf            func(string) ([]byte, error)
		want          string
	}{
		{"CABOOSE_TZ wins", "Asia/Kolkata", "UTC", link("/usr/share/zoneinfo/X"), noFile, "Asia/Kolkata"},
		{"then TZ", "", "Europe/Paris", link("/usr/share/zoneinfo/X"), noFile, "Europe/Paris"},
		{"linux link", "", "", link("/usr/share/zoneinfo/America/Sao_Paulo"), noFile, "America/Sao_Paulo"},
		{"relative link", "", "", link("../usr/share/zoneinfo/Europe/Berlin"), noFile, "Europe/Berlin"},
		{"macOS link", "", "", link("/var/db/timezone/zoneinfo/America/New_York"), noFile, "America/New_York"},
		{"last zoneinfo wins", "", "", link("/a/zoneinfo/b/zoneinfo/Etc/UTC"), noFile, "Etc/UTC"},
		{"debian file", "", "", noLink, file(" Europe/Lisbon \nignored\n"), "Europe/Lisbon"},
		{"link elsewhere falls to file", "", "", link("/elsewhere"), file("Asia/Tokyo\n"), "Asia/Tokyo"},
		{"nothing", "", "", noLink, noFile, ""},
	}
	for _, tc := range cases {
		if got := HostTimezone(tc.ctz, tc.tz, tc.rl, tc.rf); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestDockerSockPath(t *testing.T) {
	cases := []struct {
		on         bool
		host, want string
	}{
		{false, "", ""}, {false, "unix:///run/user/1/docker.sock", ""},
		{true, "", "/var/run/docker.sock"},
		{true, "tcp://x:2375", "/var/run/docker.sock"},
		{true, "unix:///run/user/1/docker.sock", "/run/user/1/docker.sock"},
	}
	for _, tc := range cases {
		if got := DockerSockPath(tc.on, tc.host); got != tc.want {
			t.Errorf("DockerSockPath(%v, %q) = %q, want %q", tc.on, tc.host, got, tc.want)
		}
	}
}

func TestCheckInsideRoot(t *testing.T) {
	const deflt = "the default: config.toml has no [roots]"
	const byEnv = "set by [roots] in /c.toml"
	one := func(host string) []config.Root { return []config.Root{{Host: host, Container: "/work"}} }
	dev := one("/h/dev")
	for _, tc := range []struct {
		dir              string
		mounted, current []config.Root
		want             string
	}{
		{"/h/dev/x", nil, dev, "/work/x"},
		{"/h/dev", dev, dev, "/work"},
		{"/anywhere", nil, one("/"), "/work/anywhere"},
		// Where the container has it, not where the configuration would.
		{"/h/dev/x", []config.Root{{Host: "/h/dev", Container: "/work/h/dev"}}, dev, "/work/h/dev/x"},
		{"/h/w/x", nil, []config.Root{{Name: "dev", Host: "/h/dev", Container: "/work/dev"}, {Name: "w", Host: "/h/w", Container: "/work/w"}}, "/work/w/x"},
	} {
		got, err := CheckInsideRoot(tc.dir, tc.mounted, tc.current, deflt, "container")
		if err != nil || got != tc.want {
			t.Errorf("CheckInsideRoot(%q, %+v, %+v) = %q, %v; want %q", tc.dir, tc.mounted, tc.current, got, err, tc.want)
		}
	}

	// Outside the one root there is: say what the root is, where its value
	// came from, and how to change it. tests/run.sh matches the first part.
	_, err := CheckInsideRoot("/h/devx", nil, dev, deflt, "container")
	want := "/h/devx is outside the mounted root, /h/dev at /work (" + deflt + ").\n" + config.RootsHelp
	if err == nil || err.Error() != want {
		t.Errorf("err = %v\nwant %s", err, want)
	}
	several := []config.Root{{Name: "a", Host: "/a", Container: "/work/a"}, {Name: "b", Host: "/b", Container: "/work/b"}}
	_, err = CheckInsideRoot("/c", nil, several, "set by [roots] in /c.toml", "container")
	want = "/c is outside every mounted root, /a at /work/a, /b at /work/b (set by [roots] in /c.toml).\n" + config.RootsHelp
	if err == nil || err.Error() != want {
		t.Errorf("err = %v\nwant %s", err, want)
	}

	// What the container has mounted wins over the configured value, and
	// a restart is the fix when the configured value holds the path...
	_, err = CheckInsideRoot("/", dev, one("/"), byEnv, "container")
	want = `/ is outside the root this container has mounted.
       mounted: /h/dev at /work (fixed when the container was created)
       current: / at /work (set by [roots] in /c.toml)
       Bind mounts cannot change under a live container: 'caboose restart'
       remounts it at the current value (this kills running sessions).`
	if err == nil || err.Error() != want {
		t.Errorf("err = %v\nwant %s", err, want)
	}
	var ee *ExitError
	if !errors.As(err, &ee) || ee.Code != 1 {
		t.Errorf("not a die(): %#v", err)
	}
	// A VM is called one, and its mounts are not bind mounts.
	_, err = CheckInsideRoot("/", dev, one("/"), byEnv, "VM")
	if err == nil || !strings.Contains(err.Error(), "outside the root this VM has mounted") ||
		!strings.Contains(err.Error(), "Mounts cannot change under a live VM") || strings.Contains(err.Error(), "container") {
		t.Errorf("under vm: %v", err)
	}
	// ...but not when it does not.
	_, err = CheckInsideRoot("/tmp/x", dev, one("/h/src"), deflt, "container")
	if err == nil || !strings.Contains(err.Error(), "outside the root this container has mounted") ||
		!strings.Contains(err.Error(), "current: /h/src at /work ("+deflt+")") ||
		!strings.Contains(err.Error(), "/tmp/x is outside the current value too") ||
		!strings.HasSuffix(err.Error(), config.RootsHelp) {
		t.Errorf("err = %v", err)
	}
}

func TestIndent(t *testing.T) {
	cases := map[string]string{"": "", "a\n": "  a\n", "a\nb\n": "  a\n  b\n", "a\nb": "  a\n  b", "\n": "  \n"}
	for in, want := range cases {
		if got := indent(in, "  "); got != want {
			t.Errorf("indent(%q) = %q, want %q", in, got, want)
		}
	}
}

// With no sandbox, status says what it would be, and asks nothing of any
// engine: there is no Docker here to ask.
func TestStatusHeaderWhenNotRunning(t *testing.T) {
	for _, tc := range []struct{ iso, noun, shown, outbound string }{
		{"", "container", "container (the default: config.toml defines no profile)", ""},
		{isolationVM, "VM       ", "vm (the default: config.toml defines no profile)",
			"outbound  : through this machine, so its VPN routes and DNS apply (egress on; egress_ports \"22 80 443\")\n"},
	} {
		var out, errb bytes.Buffer
		a := &App{
			Cfg: &config.Config{Env: "default", Container: "caboose-x", Image: "img", Roots: []config.Root{{Host: "/h/dev", Container: "/work"}},
				DataDir: "/h/.caboose", KeepVersions: 2, Egress: true, Isolation: tc.iso, Getenv: func(string) string { return "" },
				ImageProfile: config.ImageProfile{Kind: config.ImageKindApko, Name: "default", Defaults: true, Packages: []string{"jq", "postgresql-17-client"}}},
			Backend: &backendtest.Fake{},
			Stdout:  &out, Stderr: &errb,
		}
		if err := a.Status(); err != nil {
			t.Fatal(err)
		}
		want := "env       : default\n" +
			tc.noun + " : caboose-x (absent)\n" +
			"image     : img\n" +
			"base      : apko.default (caboose's packages + 2 of yours)\n" +
			"root      : /h/dev -> /work\n" +
			"data dir  : /h/.caboose\n" +
			"keeps     : ~/.claude, ~/.claude.json, ~/.config/caboose, ~/.config/git, ~/.config/jj, ~/.config/gh, ~/.ssh (in /h/.caboose/home)\n" +
			"isolation : " + tc.shown + "\n" + tc.outbound +
			"version   : " + version.Get().Version + "\n" +
			"\nnot running — start it by running caboose in a repo.\n"
		if out.String() != want {
			t.Errorf("status =\n%s\nwant\n%s", out.String(), want)
		}

		out.Reset()
		a.Cfg.Roots = []config.Root{{Name: "a", Host: "/a", Container: "/work/a"}, {Name: "dev", Host: "/h/dev", Container: "/work/dev"}}
		if err := a.Status(); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), "\nroot      : /a -> /work/a\nroot      : /h/dev -> /work/dev\n") {
			t.Errorf("status with two roots:\n%s", out.String())
		}
	}
}

func makeCheckout(t *testing.T, module string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sandbox"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("// c\nmodule "+module+"\n\ngo 1.27\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, assets.SandboxInstructionsPath), []byte("live @@CABOOSE_SOURCE@@\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestCheckoutDiscovery(t *testing.T) {
	good := makeCheckout(t, ModulePath)
	if !IsCheckout(good) {
		t.Error("a checkout was not recognised")
	}
	if IsCheckout(makeCheckout(t, "example.com/other")) {
		t.Error("another module was taken for a checkout")
	}
	if IsCheckout(t.TempDir()) {
		t.Error("an empty dir was taken for a checkout")
	}
	want, _ := filepath.EvalSymlinks(good)
	if got := FindCheckout(good); got != want {
		t.Errorf("FindCheckout(stamped) = %q, want %q", got, want)
	}
	// The test binary lives in a temp dir, far from any checkout -- just as
	// a release or `go install` binary does -- so with nothing stamped, or
	// a stamp that no longer holds a checkout, there is none.
	if got := FindCheckout(""); got != "" {
		t.Errorf("FindCheckout(unstamped) = %q", got)
	}
	if got := FindCheckout(t.TempDir()); got != "" {
		t.Errorf("FindCheckout(nothing) = %q", got)
	}
	if got := FindCheckout(filepath.Join(t.TempDir(), "gone")); got != "" {
		t.Errorf("FindCheckout(missing) = %q", got)
	}
	// A stamp that reaches the checkout through a symlink reports where it
	// really is: that path is what the container has under /work.
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(good, link); err != nil {
		t.Fatal(err)
	}
	if got := FindCheckout(link); got != want {
		t.Errorf("FindCheckout(symlink) = %q, want %q", got, want)
	}
}

func TestConfirmed(t *testing.T) {
	cases := map[string]bool{
		"y\n": true, "YES\n": true, " y \n": true,
		"\n": false, "n\n": false, "": false,
		// No newline: the read hit EOF (Ctrl-D), which is an empty reply.
		"y": false, "yes": false,
	}
	for in, want := range cases {
		if got := confirmed(strings.NewReader(in)); got != want {
			t.Errorf("confirmed(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestEnterProjectDir(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "f")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.EvalSymlinks(dir)
	if got, err := enterProjectDir(dir); err != nil || got != want {
		t.Errorf("got %q, %v", got, err)
	}
	_, err := enterProjectDir(file)
	var ee *ExitError
	if !errors.As(err, &ee) || ee.Code != 1 || ee.Msg != "cd: "+file+": Not a directory" {
		t.Errorf("err = %#v", err)
	}
	if _, err := enterProjectDir(filepath.Join(dir, "nope")); err == nil {
		t.Error("a missing dir was accepted")
	}
}

func TestReportUsageFailsWithDu(t *testing.T) {
	var out, errb bytes.Buffer
	a := &App{Cfg: &config.Config{DataDir: filepath.Join(t.TempDir(), "missing")}, Stdout: &out, Stderr: &errb}
	err := a.reportUsage("caboose: now using ", filepath.Join(a.Cfg.DataDir, "local/linux-x64"))
	var ee *ExitError
	if !errors.As(err, &ee) || ee.Code != 1 || ee.Msg != "" {
		t.Errorf("err = %#v", err)
	}
	if out.Len() != 0 || errb.Len() != 0 {
		t.Errorf("stdout %q stderr %q", out.String(), errb.String())
	}

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "local/linux-x64/share/claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	a.Cfg.DataDir = dir
	if err := a.reportUsage("caboose: now using ", filepath.Join(dir, "local/linux-x64")); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.String(), "caboose: now using ") || !strings.Contains(out.String(), dir) {
		t.Errorf("stdout %q", out.String())
	}
}

// createContainer's mounts: every source exists by the time the backend
// creates the sandbox, and the configs tools rewrite are mounted as
// directories.
func TestCreateContainerMounts(t *testing.T) {
	for _, iso := range isolations {
		t.Run(iso, func(t *testing.T) {
			b := newBoxApp(t, iso, &backendtest.Fake{})
			tmp, data := b.tmp, b.data
			var roots []config.Root
			for _, name := range []string{"dev", "w"} {
				roots = append(roots, config.Root{Name: name, Host: filepath.Join(tmp, name), Container: "/work/" + name})
				if err := os.Mkdir(filepath.Join(tmp, name), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			b.Cfg.Roots = roots
			// The user's own entries, beside the defaults.
			sbx := filepath.Join(data, datadir.SandboxConfig)
			if err := os.MkdirAll(filepath.Dir(sbx), 0o700); err != nil {
				t.Fatal(err)
			}
			extra := "\n[[keep]]\npath = \"~/.aws\"\n\n[[keep]]\npath = \"~/.config/foo\"\n"
			if err := os.WriteFile(sbx, append(sandboxcfg.Default(nil), extra...), 0o644); err != nil {
				t.Fatal(err)
			}
			// What each mount's source was when the backend was asked.
			kinds := map[string]string{}
			b.box.OnCreate = func(s backend.Spec) error {
				for _, m := range s.Mounts {
					fi, err := os.Stat(m.Source)
					switch {
					case err != nil:
						kinds[m.Source] = "MISSING"
					case fi.IsDir():
						kinds[m.Source] = "dir"
					default:
						kinds[m.Source] = "file"
					}
				}
				return nil
			}
			if err := b.createContainer(false); err != nil {
				t.Fatalf("createContainer: %v\n%s", err, b.errb)
			}
			spec, _ := b.box.Spec()
			got := map[string]string{}
			var rootMounts []backend.Mount
			for _, m := range spec.Mounts {
				if kinds[m.Source] == "MISSING" {
					t.Errorf("mount source missing at creation: %+v", m)
				}
				// caboose's instructions alone are read-only: the
				// sandbox reads them, and writes everything else.
				if m.ReadOnly != (m.Target == datadir.ManagedTarget) {
					t.Errorf("%s: read-only %v", m.Target, m.ReadOnly)
				}
				if strings.HasPrefix(m.Target, "/work") {
					rootMounts = append(rootMounts, m)
				}
				if rel, ok := strings.CutPrefix(m.Source, data+"/"); ok {
					got[m.Target] = kinds[m.Source] + " " + rel
				}
			}
			want := map[string]string{
				"/home/agent/.claude":             "dir home/.claude",
				"/home/agent/.claude.json":        "file home/.claude.json",
				"/home/agent/.local/bin":          "dir local/linux-arm64/bin",
				"/home/agent/.local/share/claude": "dir local/linux-arm64/share/claude",
				"/home/agent/.cache/claude":       "dir local/linux-arm64/cache/claude",
				"/home/agent/.config/git":         "dir home/.config/git",
				"/home/agent/.config/jj":          "dir home/.config/jj",
				"/home/agent/.config/gh":          "dir home/.config/gh",
				"/home/agent/.ssh":                "dir home/.ssh",
				"/home/agent/.caboose-sync":       "dir sync",
				"/home/agent/.caboose-proposals":  "dir proposals",
				"/home/agent/.config/caboose":     "dir home/.config/caboose",
				"/home/agent/.aws":                "dir home/.aws",
				"/home/agent/.config/foo":         "dir home/.config/foo",
				"/etc/claude-code":                "dir claude-code",
			}
			for dst, w := range want {
				if got[dst] != w {
					t.Errorf("%s: mounted %q, want %q", dst, got[dst], w)
				}
			}
			if len(got) != len(want) {
				t.Errorf("data dir mounts = %v", got)
			}
			// Every home mount of caboose's own, beside the keep entries, is one a
			// keep entry may not name: sandboxcfg.Reserved has to keep up with
			// this function.
			for dst, w := range got {
				rel, inHome := strings.CutPrefix(dst, config.ContainerHome+"/")
				if strings.Contains(w, " home/") || !inHome {
					continue
				}
				if !slices.Contains(sandboxcfg.Reserved, rel) {
					t.Errorf("~/%s is mounted by caboose but a keep entry may name it: add it to sandboxcfg.Reserved", rel)
				}
			}
			for _, p := range []string{"home/.aws", "home/.config/foo", "home/.ssh", "home/.config/gh"} {
				if fi, err := os.Stat(filepath.Join(data, p)); err != nil || fi.Mode().Perm() != 0o700 {
					t.Errorf("%s: %v, want a 0700 dir", p, fi)
				}
			}
			// Each root at /work/<name>, whatever its host path.
			if w := []backend.Mount{mount(tmp+"/dev", "/work/dev"), mount(tmp+"/w", "/work/w")}; !reflect.DeepEqual(rootMounts, w) {
				t.Errorf("root mounts = %v, want %v", rootMounts, w)
			}
		})
	}
}
