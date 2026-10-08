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
		ImageProfile: ImageProfile{Kind: ImageKindApko, Name: "default", Defaults: true},
		Egress:       true, EgressPorts: DefaultEgressPorts}
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
			if ref := c.BaseRef(); ref != "caboose-base:"+tc.env {
				t.Errorf("base %q", ref)
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

// A ref may be any image but the environment's own.
func TestCheckImages(t *testing.T) {
	for _, tc := range []struct {
		ref string
		ok  bool
	}{
		{"caboose-base:default", true},
		{"node:22", true},
		{"caboose:default", false},
		{"docker.io/library/caboose:default", false},
	} {
		c := &Config{Env: "default", Image: ImageFor("default"), ImageProfile: ImageProfile{Kind: ImageKindRef, Name: "a", Ref: tc.ref}}
		if err := c.CheckImages(); (err == nil) != tc.ok {
			t.Errorf("%+v: %v", tc, err)
		} else if err != nil && !strings.Contains(err.Error(), "is the environment's own image") {
			t.Errorf("%+v: %v", tc, err)
		}
	}
	// Other kinds build their own base.
	c := &Config{Env: "default", Image: ImageFor("default"), ImageProfile: DefaultImageProfile()}
	if err := c.CheckImages(); err != nil {
		t.Error(err)
	}
}

// The image profile is chosen as the isolation profile is: image names
// it; else the only one defined; else apko.default, which image may name
// without a table.
func TestImageProfile(t *testing.T) {
	for name, tc := range map[string]struct {
		body string
		want ImageProfile
	}{
		"nothing":          {"", ImageProfile{Kind: ImageKindApko, Name: "default", Defaults: true}},
		"implicit":         {`image = "apko.default"`, ImageProfile{Kind: ImageKindApko, Name: "default", Defaults: true}},
		"the one":          {"[ref.mine]\nimage = \"node:22\"\n", ImageProfile{Kind: ImageKindRef, Name: "mine", Ref: "node:22"}},
		"named":            {"image = \"apko.min\"\n[apko.min]\ndefaults = false\npackages = [\"jq\", \"go-1.26\"]\n[ref.mine]\nimage = \"x\"\n", ImageProfile{Kind: ImageKindApko, Name: "min", Packages: []string{"jq", "go-1.26"}}},
		"dockerfile":       {"[dockerfile.default]\n", ImageProfile{Kind: ImageKindDockerfile, Name: "default", Dir: "/h/.caboose/envs/default/dockerfile/default"}},
		"named dockerfile": {"image = \"dockerfile.x\"\n[dockerfile.x]\n[ref.y]\nimage = \"z\"\n", ImageProfile{Kind: ImageKindDockerfile, Name: "x", Dir: "/h/.caboose/envs/default/dockerfile/x"}},
		"with apko":        {"image = \"apko.default\"\n[apko.default]\npackages = []\n", ImageProfile{Kind: ImageKindApko, Name: "default", Defaults: true}},
	} {
		t.Run(name, func(t *testing.T) {
			c, err := load(t, tc.body)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(c.ImageProfile, tc.want) {
				t.Errorf("got %+v, want %+v", c.ImageProfile, tc.want)
			}
		})
	}
}

func TestImageProfileRefuses(t *testing.T) {
	for name, tc := range map[string]struct{ body, err string }{
		"several":          {"[ref.a]\nimage = \"x\"\n[dockerfile.b]\n", `it defines the image profiles dockerfile.b, ref.a, and image does not say which to use: set image = "dockerfile.b", say`},
		"an isolation":     {"image = \"vm.default\"\n[vm.default]\n", `image = "vm.default" is not an image profile, "<kind>.<name>" of a kind apko, dockerfile, ref`},
		"no kind":          {`image = "default"`, `image = "default" is not an image profile`},
		"undefined":        {"image = \"ref.x\"\n", `image = "ref.x" names a profile the file does not define (none is): add a [ref.x] table, or name another`},
		"undefined apko":   {"image = \"apko.min\"\n[ref.a]\nimage = \"x\"\n", `image = "apko.min" names a profile the file does not define (ref.a are)`},
		"bad package":      {"[apko.default]\npackages = [\"Bad Name\"]\n", `packages in [apko.default]: package name "Bad Name" has the character`},
		"empty package":    {"[apko.default]\npackages = [\"\"]\n", `packages in [apko.default] must be an array of strings`},
		"apko key":         {"[apko.default]\ndir = \"x\"\n", "unknown setting in [apko.default] dir (known: defaults, packages;"},
		"dockerfile key":   {"[dockerfile.default]\npackages = []\n", "unknown setting in [dockerfile.default] packages (it takes none"},
		"ref key":          {"[ref.default]\nbase = \"x\"\n", "unknown setting in [ref.default] base (known: image;"},
		"ref without one":  {"[ref.default]\n", "[ref.default] needs image"},
		"dir":              {"[dockerfile.x]\ndir = \"/srv/df\"\n", "unknown setting in [dockerfile.x] dir (it takes none"},
		"undefined docker": {"image = \"dockerfile.x\"\n", `image = "dockerfile.x" names a profile the file does not define (none is): add a [dockerfile.x] table`},
		"bad profile name": {"[apko.Big]\n", "'Big' is not a profile name"},
		"old [image]":      {"[image]\nauto_build = false\n", "image must be a string"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := load(t, tc.body)
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("err = %v, want %q", err, tc.err)
			}
		})
	}
}

// [build] auto_build is read; it is on by default.
func TestAutoBuild(t *testing.T) {
	if c, err := load(t, ""); err != nil || !c.AutoBuild {
		t.Errorf("default: %v", err)
	}
	if c, err := load(t, "[build]\nauto_build = false\n"); err != nil || c.AutoBuild {
		t.Errorf("off: %v", err)
	}
}

// An apko profile's lock is next to config.toml, named after the profile;
// no other kind has one.
func TestLockPath(t *testing.T) {
	c, err := load(t, "image = \"apko.min\"\n[apko.min]\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := c.LockPath(); got != "/h/.caboose/envs/default/apko-min.lock.json" {
		t.Errorf("LockPath = %q", got)
	}
	if got := c.BaseRef(); got != "caboose-base:default" {
		t.Errorf("BaseRef = %q", got)
	}
	c, err = load(t, "[ref.a]\nimage = \"node:22\"\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := c.LockPath(); got != "" {
		t.Errorf("LockPath of a ref = %q", got)
	}
	if got := c.BaseRef(); got != "node:22" {
		t.Errorf("BaseRef = %q", got)
	}
}

// ReadImage resolves the image again from a reread File.
func TestReadImage(t *testing.T) {
	c, err := load(t, "")
	if err != nil {
		t.Fatal(err)
	}
	if c.File, err = ParseFile(cfgPath, []byte("[ref.a]\nimage = \"x\"\n")); err != nil {
		t.Fatal(err)
	}
	if err := c.ReadImage(); err != nil || c.ImageProfile.String() != "ref.a" {
		t.Errorf("%v %v", c.ImageProfile, err)
	}
	c.File = nil
	if err := c.ReadImage(); err != nil || c.ImageProfile.String() != "apko.default" {
		t.Errorf("%v %v", c.ImageProfile, err)
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
