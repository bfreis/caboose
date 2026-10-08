package config

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

type fakeFS map[string]string // path -> "dir", or a file's contents

func (f fakeFS) IsDir(p string) bool { return f[p] == "dir" }
func (f fakeFS) ReadFile(p string) ([]byte, error) {
	v, ok := f[p]
	switch {
	case !ok:
		return nil, fs.ErrNotExist
	case v == "dir":
		return nil, errors.New("is a directory")
	}
	return []byte(v), nil
}

func envOf(m map[string]string) Env { return func(k string) string { return m[k] } }

func TestDataDirResolution(t *testing.T) {
	const dst = "/h/.caboose/envs/default/data"
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"default", nil, dst},
		{"explicit", map[string]string{"CABOOSE_DATA_DIR": "/x"}, "/x"},
		{"CABOOSE_HOME", map[string]string{"CABOOSE_HOME": "/c"}, "/c/envs/default/data"},
		{"another env", map[string]string{"CABOOSE_ENV": "work"}, "/h/.caboose/envs/work/data"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]string{"HOME": "/h"}
			for k, v := range tc.env {
				env[k] = v
			}
			c, err := Load(envOf(env), fakeFS{}, "")
			if err != nil {
				t.Fatal(err)
			}
			if c.DataDir != tc.want {
				t.Errorf("DataDir = %q, want %q", c.DataDir, tc.want)
			}
		})
	}
}

// Only the default environment exists without being created.
func TestCheckEnv(t *testing.T) {
	fs := fakeFS{"/h/.caboose/envs/work": "dir"}
	for env, ok := range map[string]bool{"default": true, "work": true, "wrok": false} {
		c, _ := Load(envOf(map[string]string{"HOME": "/h"}), fs, env)
		err := c.CheckEnv(fs)
		if (err == nil) != ok {
			t.Errorf("%s: %v", env, err)
		}
		if err != nil && !strings.Contains(err.Error(), "caboose -e wrok setup") {
			t.Errorf("%s: %v", env, err)
		}
	}
}

// Two roots where one holds the other would give a project two container
// paths; siblings sharing a prefix do not overlap.
func TestResolveRootsRefusesOverlap(t *testing.T) {
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	for _, d := range []string{"a/b", "ab"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	root := func(name, p string) Root {
		return Root{Name: name, Host: filepath.Join(dir, p), Container: "/work/" + name}
	}
	c := &Config{Roots: []Root{root("a", "a"), root("ab", "ab")}}
	if err := c.ResolveRoots(); err != nil {
		t.Errorf("siblings: %v", err)
	}
	for _, roots := range [][]Root{
		{root("a", "a"), root("b", "a/b")},
		{root("b", "a/b"), root("a", "a")},
		{root("a", "a"), root("same", "a")},
	} {
		c := &Config{Roots: roots}
		err := c.ResolveRoots()
		if err == nil || !strings.Contains(err.Error(), "overlap") {
			t.Errorf("%+v: err = %v", roots, err)
		}
	}
}

func TestWithin(t *testing.T) {
	cases := []struct {
		path, root string
		want       bool
	}{
		{"/h/dev", "/h/dev", true},
		{"/h/dev/x", "/h/dev", true},
		{"/h/devx", "/h/dev", false},
		{"/h", "/h/dev", false},
		{"/anything", "/", true},
		{"/", "/", true},
	}
	for _, tc := range cases {
		if got := Within(tc.path, tc.root); got != tc.want {
			t.Errorf("Within(%q, %q) = %v", tc.path, tc.root, got)
		}
	}
}

func TestContainerPath(t *testing.T) {
	one := []Root{{Host: "/h/dev", Container: "/work"}}
	slash := []Root{{Host: "/", Container: "/work"}}
	several := []Root{{Name: "dev", Host: "/h/dev", Container: "/work/dev"}, {Name: "w", Host: "/h/w", Container: "/work/w"}}
	cases := []struct {
		roots []Root
		in    string
		want  string
		ok    bool
	}{
		{one, "/h/dev", "/work", true},
		{one, "/h/dev/you/caboose", "/work/you/caboose", true},
		{one, "/h/devx", "", false},
		{one, "/h", "", false},
		{slash, "/", "/work", true},
		{slash, "/h/c", "/work/h/c", true},
		{several, "/h/dev/x", "/work/dev/x", true},
		{several, "/h/w", "/work/w", true},
		{several, "/h/other", "", false},
	}
	for _, tc := range cases {
		got, ok := ContainerPath(tc.roots, tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("ContainerPath(%+v, %q) = %q, %v; want %q, %v", tc.roots, tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestSameRoots(t *testing.T) {
	a := Root{Name: "a", Host: "/a", Container: "/work/a"}
	b := Root{Name: "b", Host: "/b", Container: "/work/b"}
	if !SameRoots([]Root{a, b}, []Root{{Host: "/b", Container: "/work/b"}, {Host: "/a", Container: "/work/a"}}) {
		t.Error("the same mounts in another order, without names, differ")
	}
	for _, other := range [][]Root{
		{a},
		{a, {Host: "/b", Container: "/work/c"}},
		{a, {Host: "/c", Container: "/work/b"}},
		{a, b, {Host: "/c", Container: "/work/c"}},
	} {
		if SameRoots([]Root{a, b}, other) {
			t.Errorf("%+v reads as the same", other)
		}
	}
}

func TestNormalizeImage(t *testing.T) {
	for in, want := range map[string]string{
		"alpine":                              "alpine:latest",
		"alpine:3":                            "alpine:3",
		"library/alpine":                      "alpine:latest",
		"docker.io/alpine":                    "alpine:latest",
		"docker.io/library/alpine:latest":     "alpine:latest",
		"index.docker.io/library/node:22":     "node:22",
		"docker.io/bfreis/caboose":            "bfreis/caboose:latest",
		"ghcr.io/x/caboose":                   "ghcr.io/x/caboose:latest",
		"localhost:5000/caboose":              "localhost:5000/caboose:latest",
		"localhost:5000/caboose:1":            "localhost:5000/caboose:1",
		"alpine@sha256:abc":                   "alpine@sha256:abc",
		"docker.io/library/alpine@sha256:abc": "alpine@sha256:abc",
	} {
		if got := NormalizeImage(in); got != want {
			t.Errorf("NormalizeImage(%q) = %q, want %q", in, got, want)
		}
	}
	if SameImage("caboose", "caboose-base") || SameImage("img", "img:base") || !SameImage("img", "docker.io/library/img:latest") {
		t.Error("SameImage")
	}
}

func TestHostPath(t *testing.T) {
	one := []Root{{Host: "/h/dev", Container: "/work"}}
	slash := []Root{{Host: "/", Container: "/work"}}
	several := []Root{{Name: "dev", Host: "/h/dev", Container: "/work/dev"}, {Name: "w", Host: "/h/w", Container: "/work/w"}}
	for _, tc := range []struct {
		roots []Root
		in    string
		want  string
		ok    bool
	}{
		{one, "/work", "/h/dev", true},
		{one, "/work/", "/h/dev", true},
		{one, "/work/you/caboose", "/h/dev/you/caboose", true},
		{one, "/work/a/../b", "/h/dev/b", true},
		{one, "/workx", "", false},
		{one, "/workx/a", "", false},
		{one, "/work/../etc", "", false},
		{one, "/work/../work/x", "/h/dev/x", true},
		{one, "/", "", false},
		{one, "work/x", "", false},
		{one, "", "", false},
		{slash, "/work/h/c", "/h/c", true},
		{slash, "/work", "/", true},
		{several, "/work/dev/x", "/h/dev/x", true},
		{several, "/work/w", "/h/w", true},
		{several, "/work", "", false},
		{several, "/work/other", "", false},
		{nil, "/work", "", false},
	} {
		got, ok := HostPath(tc.roots, tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("HostPath(%+v, %q) = %q, %v; want %q, %v", tc.roots, tc.in, got, ok, tc.want, tc.ok)
		}
		if ok {
			// And back again.
			if back, ok := ContainerPath(tc.roots, got); !ok || back != path.Clean(tc.in) {
				t.Errorf("ContainerPath(%q) = %q, %v; want %q", got, back, ok, path.Clean(tc.in))
			}
		}
	}
}

func TestDefaults(t *testing.T) {
	c, err := Load(envOf(map[string]string{"HOME": "/h"}), fakeFS{}, "")
	if err != nil {
		t.Fatal(err)
	}
	want := Config{Env: "default", CabooseHome: "/h/.caboose", EnvDir: "/h/.caboose/envs/default",
		Image: "caboose:default", Container: "caboose-default", DataDir: "/h/.caboose/envs/default/data",
		Roots:     []Root{{Name: "dev", Host: "/h/dev", Container: "/work/dev"}},
		AutoBuild: true, Tmux: true, KeepVersions: 2, ReadyTimeout: 600, Home: "/h",
		ForwardPorts: DefaultForwardPorts, OpenURLs: "ask", Isolation: KindContainer,
		Egress: true, EgressPorts: DefaultEgressPorts}
	c.Getenv = nil
	if !reflect.DeepEqual(*c, want) {
		t.Errorf("got %+v\nwant %+v", *c, want)
	}
	if c.RootsOrigin() != "the default: config.toml has no [roots]" {
		t.Errorf("origin %q", c.RootsOrigin())
	}
}

// An environment is a whole caboose: its own dir, container and images,
// named after it, the default one's too.
func TestEnvironments(t *testing.T) {
	for _, tc := range []struct {
		name, flag, envVar         string
		env, dir, container, image string
	}{
		{"default", "", "", "default", "/h/.caboose/envs/default", "caboose-default", "caboose:default"},
		{"CABOOSE_ENV", "", "work", "work", "/h/.caboose/envs/work", "caboose-work", "caboose:work"},
		{"--env", "play", "", "play", "/h/.caboose/envs/play", "caboose-play", "caboose:play"},
		{"--env wins", "play", "work", "play", "/h/.caboose/envs/play", "caboose-play", "caboose:play"},
		{"naming default", "default", "work", "default", "/h/.caboose/envs/default", "caboose-default", "caboose:default"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Load(envOf(map[string]string{"HOME": "/h", "CABOOSE_ENV": tc.envVar}), fakeFS{}, tc.flag)
			if err != nil {
				t.Fatal(err)
			}
			if c.Env != tc.env || c.EnvDir != tc.dir || c.Container != tc.container || c.Image != tc.image {
				t.Errorf("env %q dir %q container %q image %q", c.Env, c.EnvDir, c.Container, c.Image)
			}
			if ref, byo := c.Base(); ref != "caboose-base:"+tc.env || byo {
				t.Errorf("base %q %v", ref, byo)
			}
		})
	}
	for _, bad := range []string{"Work", "-x", "a/b", "..", "a b", "a.b", "a:b", strings.Repeat("a", 33)} {
		if _, err := Load(envOf(map[string]string{"HOME": "/h"}), fakeFS{}, bad); err == nil {
			t.Errorf("env %q accepted", bad)
		}
	}
}

// Every environment name makes a valid docker image tag and container
// name: the tag's rule, [A-Za-z0-9_][A-Za-z0-9_.-]{0,127}, holds the
// environment's.
func TestEnvNamesAreDockerNames(t *testing.T) {
	tag := regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
	container := regexp.MustCompile(`^/?[a-zA-Z0-9][a-zA-Z0-9_.-]+$`)
	for _, env := range []string{"default", "a", "0", "a-b_c", strings.Repeat("z", 32)} {
		if !ValidEnv(env) || !tag.MatchString(env) || !container.MatchString(ContainerFor(env)) {
			t.Errorf("%q", env)
		}
	}
	if !strings.Contains(envName.String(), "^[a-z0-9][a-z0-9_-]{0,31}$") {
		t.Errorf("the environment's rule changed (%s): check it against docker's again", envName)
	}
}

func TestResolveRoots(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	c := &Config{Roots: []Root{{Name: "dev", Host: link, Container: "/work/dev"}}}
	if err := c.ResolveRoots(); err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.EvalSymlinks(real)
	if c.Roots[0].Host != want || c.Roots[0].Container != "/work/dev" {
		t.Errorf("Roots = %+v, want %q at /work/dev", c.Roots, want)
	}
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	nope := filepath.Join(dir, "nope")
	for _, tc := range []struct {
		root  Root
		from  string
		first string
	}{
		// The first run of someone whose repos are not in ~/dev.
		{Root{Name: "dev", Host: nope}, "", "root dev, " + nope + ", does not exist (the default: config.toml has no [roots])."},
		{Root{Name: "dev", Host: file}, "/c.toml", "root dev, " + file + ", is not a directory (set by [roots] in /c.toml)."},
	} {
		c := &Config{Roots: []Root{tc.root}, RootsFrom: tc.from}
		err := c.ResolveRoots()
		if err == nil {
			t.Errorf("%+v: no error", tc)
			continue
		}
		first, rest, _ := strings.Cut(err.Error(), "\n")
		if first != tc.first {
			t.Errorf("first line %q\nwant       %q", first, tc.first)
		}
		// Every variant says what the roots are and how to change them.
		if rest != RootsHelp {
			t.Errorf("%+v: explanation:\n%s", tc, rest)
		}
	}
	for _, want := range []string{"/work/<name>", "[roots]", "caboose setup roots", "'caboose restart'"} {
		if !strings.Contains(RootsHelp, want) {
			t.Errorf("RootsHelp lacks %q", want)
		}
	}
}

func TestEmptyHome(t *testing.T) {
	for _, env := range []map[string]string{{}, {"HOME": ""}, {"CABOOSE_DATA_DIR": "/d"}} {
		c, err := Load(envOf(env), fakeFS{}, "")
		if err == nil || !strings.Contains(err.Error(), "HOME is not set") || c != nil {
			t.Errorf("%v: c=%+v err=%v", env, c, err)
		}
	}
	// Nothing left that defaults under HOME: fine without it.
	c, err := Load(envOf(map[string]string{"CABOOSE_HOME": "/c", "CABOOSE_DATA_DIR": "/d"}),
		fakeFS{"/c/envs/default/config.toml": "[roots]\na = \"/a\"\n"}, "")
	if err != nil || c.DataDir != "/d" || c.Roots[0].Host != "/a" {
		t.Errorf("c=%+v err=%v", c, err)
	}
}

func TestDescribeRoots(t *testing.T) {
	got := DescribeRoots([]Root{{Host: "/a", Container: "/work/a"}, {Host: "/h/t", Container: "/opt/t"}})
	if want := "/a at /work/a, /h/t at /opt/t"; got != want {
		t.Errorf("%q, want %q", got, want)
	}
}

// [image] base may be any image but the environment's own.
func TestCheckImages(t *testing.T) {
	for _, tc := range []struct {
		base string
		ok   bool
	}{
		{"", true},
		{"caboose-base:default", true},
		{"node:22", true},
		{"caboose:default", false},
		{"docker.io/library/caboose:default", false},
	} {
		c := &Config{Env: "default", Image: ImageFor("default"), BaseImage: tc.base}
		if err := c.CheckImages(); (err == nil) != tc.ok {
			t.Errorf("%+v: %v", tc, err)
		} else if err != nil && !strings.Contains(err.Error(), "is the environment's own image") {
			t.Errorf("%+v: %v", tc, err)
		}
	}
}

// An environment's image/ dir is its base, found by Load; with [image]
// base as well, which to build on would be a guess.
func TestImageDir(t *testing.T) {
	fs := fakeFS{"/h/.caboose/envs/work": "dir", "/h/.caboose/envs/work/image": "dir", "/h/.caboose/envs/default/image": "file"}
	c, err := Load(envOf(map[string]string{"HOME": "/h"}), fs, "work")
	if err != nil || c.ImageDir != "/h/.caboose/envs/work/image" {
		t.Fatalf("ImageDir %q, %v", c.ImageDir, err)
	}
	if ref, byo := c.Base(); ref != "caboose-base:work" || byo {
		t.Errorf("Base = %q, %v", ref, byo)
	}
	if err := c.CheckImages(); err != nil {
		t.Error(err)
	}
	c.BaseImage = "node:22"
	if ref, byo := c.Base(); ref != "node:22" || !byo {
		t.Errorf("Base = %q, %v", ref, byo)
	}
	if err := c.CheckImages(); !errors.Is(err, ErrTwoBases) || !strings.Contains(err.Error(), "remove base from /h/.caboose/envs/work/config.toml") {
		t.Errorf("both: %v", err)
	}
	// Not a directory: not an image dir.
	if c, _ := Load(envOf(map[string]string{"HOME": "/h"}), fs, "default"); c.ImageDir != "" {
		t.Errorf("default: ImageDir %q", c.ImageDir)
	}
}

// The boolean variables take one spelling set, strictly.
func TestEnvBool(t *testing.T) {
	for v, want := range map[string]bool{"": false, "0": false, "false": false, "No": false, "OFF": false,
		"1": true, "true": true, "YES": true, "on": true, "True": true} {
		got, err := EnvBool(envOf(map[string]string{"X": v}), "X")
		if err != nil || got != want {
			t.Errorf("%q: %v %v", v, got, err)
		}
	}
	for _, v := range []string{"2", "y", "enabled", " 1"} {
		if _, err := EnvBool(envOf(map[string]string{"X": v}), "X"); err == nil || !strings.Contains(err.Error(), "X=") {
			t.Errorf("%q: %v", v, err)
		}
	}
	c, err := Load(envOf(map[string]string{"HOME": "/h", "CABOOSE_FORCE": "yes", "CABOOSE_NO_AUTO_UPDATE": "1", "CABOOSE_SESSION": "two"}), fakeFS{}, "")
	if err != nil || !c.Force || !c.NoAutoUpdate || c.Session != "two" {
		t.Errorf("%+v %v", c, err)
	}
	if _, err := Load(envOf(map[string]string{"HOME": "/h", "CABOOSE_FORCE": "please"}), fakeFS{}, ""); err == nil || !strings.Contains(err.Error(), "CABOOSE_FORCE") {
		t.Errorf("CABOOSE_FORCE=please: %v", err)
	}
	if c, err := Machine(envOf(map[string]string{"HOME": "/h", "CABOOSE_NO_AUTO_UPDATE": "on"})); err != nil || !c.NoAutoUpdate || c.CabooseHome != "/h/.caboose" {
		t.Errorf("Machine: %+v %v", c, err)
	}
}

// A hostname that is no single lowercase DNS label is refused at load,
// saying where it came from.
func TestHostnameSetting(t *testing.T) {
	for _, good := range []string{"a", "caboose-laptop", "a1", strings.Repeat("a", 63)} {
		if err := CheckHostname(good); err != nil {
			t.Errorf("CheckHostname(%q): %v", good, err)
		}
	}
	for _, bad := range []string{"Laptop", "-a", "a-", "a.b", "a_b", "a b", strings.Repeat("a", 64)} {
		if CheckHostname(bad) == nil {
			t.Errorf("CheckHostname(%q) accepted", bad)
		}
		_, err := Load(envOf(map[string]string{"HOME": "/h"}), fakeFS{cfgPath: "[session]\nhostname = \"" + bad + "\"\n"}, "")
		if err == nil || !strings.Contains(err.Error(), cfgPath+": hostname in [session]") {
			t.Errorf("hostname %q: %v", bad, err)
		}
	}
}
