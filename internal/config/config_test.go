package config

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"reflect"
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

func TestDefaults(t *testing.T) {
	c, _ := Load(envOf(map[string]string{"HOME": "/h"}), fakeFS{}, "")
	want := Config{Env: "default", CabooseHome: "/h/.caboose", EnvDir: "/h/.caboose/envs/default",
		Image: "caboose", Container: "caboose", DataDir: "/h/.caboose/envs/default/data",
		Roots:        []Root{{Host: "/h/dev", Container: "/work"}},
		ReadyTimeout: "600", KeepVersions: "2", Home: "/h",
		ForwardPorts: DefaultForwardPorts, OpenURLs: "ask", Isolation: "docker",
		EgressProxy: "on", EgressPorts: DefaultEgressPorts}
	c.Getenv = nil
	if !reflect.DeepEqual(*c, want) {
		t.Errorf("got %+v\nwant %+v", *c, want)
	}
}

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

// An environment is a whole caboose: its own dir, container and image. The
// default one's are named plain caboose.
func TestEnvironments(t *testing.T) {
	for _, tc := range []struct {
		name, flag, envVar         string
		env, dir, container, image string
	}{
		{"default", "", "", "default", "/h/.caboose/envs/default", "caboose", "caboose"},
		{"CABOOSE_ENV", "", "work", "work", "/h/.caboose/envs/work", "caboose-work", "caboose-work"},
		{"--env", "play", "", "play", "/h/.caboose/envs/play", "caboose-play", "caboose-play"},
		{"--env wins", "play", "work", "play", "/h/.caboose/envs/play", "caboose-play", "caboose-play"},
		{"naming default", "default", "work", "default", "/h/.caboose/envs/default", "caboose", "caboose"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Load(envOf(map[string]string{"HOME": "/h", "CABOOSE_ENV": tc.envVar}), fakeFS{}, tc.flag)
			if err != nil {
				t.Fatal(err)
			}
			if c.Env != tc.env || c.EnvDir != tc.dir || c.Container != tc.container || c.Image != tc.image {
				t.Errorf("env %q dir %q container %q image %q", c.Env, c.EnvDir, c.Container, c.Image)
			}
		})
	}
	// The variables still name them outright.
	c, _ := Load(envOf(map[string]string{"HOME": "/h", "CABOOSE_CONTAINER": "box", "CABOOSE_IMAGE": "img"}), fakeFS{}, "work")
	if c.Container != "box" || c.Image != "img" {
		t.Errorf("container %q image %q", c.Container, c.Image)
	}
	for _, bad := range []string{"Work", "-x", "a/b", "..", "a b", strings.Repeat("a", 33)} {
		if _, err := Load(envOf(map[string]string{"HOME": "/h"}), fakeFS{}, bad); err == nil {
			t.Errorf("env %q accepted", bad)
		}
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
	c := &Config{Roots: []Root{{Host: link, Container: "/work"}}}
	if err := c.ResolveRoots(); err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.EvalSymlinks(real)
	if c.Roots[0].Host != want || c.Roots[0].Container != "/work" {
		t.Errorf("Roots = %+v, want %q at /work", c.Roots, want)
	}
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	nope := filepath.Join(dir, "nope")
	cfg := &File{Path: "/c.toml"}
	cases := []struct {
		root  Root
		from  string
		first string
	}{
		// The first run of someone whose repos are not in ~/dev.
		{Root{Host: nope}, "", "repo root " + nope + " does not exist (the default: CABOOSE_REPO_ROOT is unset)."},
		{Root{Host: nope}, "CABOOSE_REPO_ROOT", "repo root " + nope + " does not exist (set by CABOOSE_REPO_ROOT)."},
		{Root{Host: file}, "CABOOSE_REPO_ROOT", "repo root " + file + " is not a directory (set by CABOOSE_REPO_ROOT)."},
		{Root{Name: "dev", Host: nope}, cfg.Path, "root dev, " + nope + ", does not exist (set by [roots] in /c.toml)."},
	}
	for _, tc := range cases {
		c := &Config{Roots: []Root{tc.root}, RootsFrom: tc.from, File: cfg}
		err := c.ResolveRoots()
		if err == nil {
			t.Errorf("%+v: no error", tc)
			continue
		}
		first, rest, _ := strings.Cut(err.Error(), "\n")
		if first != tc.first {
			t.Errorf("first line %q\nwant       %q", first, tc.first)
		}
		// Every variant says what the root is and how to change it.
		if rest != RepoRootHelp {
			t.Errorf("%+v: explanation:\n%s", tc, rest)
		}
	}
	for _, want := range []string{
		"mounted into the container, at\n       /work",
		"export CABOOSE_REPO_ROOT=/path/holding/your/projects",
		"[roots]",
		"'caboose restart'",
	} {
		if !strings.Contains(RepoRootHelp, want) {
			t.Errorf("RepoRootHelp lacks %q", want)
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

func TestRootsFrom(t *testing.T) {
	cases := []struct {
		env  map[string]string
		root string
		from string
	}{
		{map[string]string{"HOME": "/h"}, "/h/dev", ""},
		{map[string]string{"HOME": "/h", "CABOOSE_REPO_ROOT": "/r"}, "/r", "CABOOSE_REPO_ROOT"},
	}
	for _, tc := range cases {
		c, err := Load(envOf(tc.env), fakeFS{}, "")
		if err != nil || len(c.Roots) != 1 || c.Roots[0] != (Root{Host: tc.root, Container: "/work"}) || c.RootsFrom != tc.from {
			t.Errorf("%v: roots=%+v from=%q err=%v", tc.env, c.Roots, c.RootsFrom, err)
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

func TestEmptyHome(t *testing.T) {
	for _, env := range []map[string]string{
		{},
		{"HOME": ""},
		{"CABOOSE_DATA_DIR": "/d"},
		{"CABOOSE_REPO_ROOT": "/r"},
	} {
		c, err := Load(envOf(env), fakeFS{}, "")
		if err == nil || !strings.Contains(err.Error(), "HOME is not set") || c != nil {
			t.Errorf("%v: c=%+v err=%v", env, c, err)
		}
	}
	// Nothing left that defaults under HOME: fine without it.
	c, err := Load(envOf(map[string]string{"CABOOSE_DATA_DIR": "/d", "CABOOSE_REPO_ROOT": "/r"}), fakeFS{}, "")
	if err != nil || c.DataDir != "/d" || c.Roots[0].Host != "/r" {
		t.Errorf("c=%+v err=%v", c, err)
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

func TestDescribeRoots(t *testing.T) {
	if got := DescribeRoots([]Root{{Host: "/h/dev", Container: "/work"}}); got != "/h/dev" {
		t.Errorf("one: %q", got)
	}
	got := DescribeRoots([]Root{{Host: "/a", Container: "/work/a"}, {Host: "/h/dev", Container: "/work/h/dev"}})
	if want := "/a at /work/a, /h/dev at /work/h/dev"; got != want {
		t.Errorf("several: %q, want %q", got, want)
	}
}

// CABOOSE_BASE_IMAGE is read as is.
func TestBaseImage(t *testing.T) {
	c, _ := Load(envOf(map[string]string{"HOME": "/h", "CABOOSE_BASE_IMAGE": "alpine:3.20"}), fakeFS{}, "")
	if c.BaseImage != "alpine:3.20" || c.Image != "caboose" {
		t.Errorf("base %q, image %q", c.BaseImage, c.Image)
	}
}

func TestDefaultBaseTag(t *testing.T) {
	for in, want := range map[string]string{
		"caboose":                     "caboose-base",
		"caboose:dev":                 "caboose-base:dev",
		"ghcr.io/x/caboose:1":         "ghcr.io/x/caboose-base:1",
		"localhost:5000/caboose":      "localhost:5000/caboose-base",
		"localhost:5000/caboose:t":    "localhost:5000/caboose-base:t",
		"caboose@sha256:0123abcd":     "caboose-base",
		"x/caboose:1@sha256:0123abcd": "x/caboose-base:1",
	} {
		if got := DefaultBaseTag(in); got != want {
			t.Errorf("DefaultBaseTag(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBase(t *testing.T) {
	c := &Config{Image: "img"}
	if ref, byo := c.Base(); ref != "img-base" || byo {
		t.Errorf("default: %q, %v", ref, byo)
	}
	c.BaseImage = "node:22"
	if ref, byo := c.Base(); ref != "node:22" || !byo {
		t.Errorf("byo: %q, %v", ref, byo)
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

// CABOOSE_BASE_IMAGE may be any image but CABOOSE_IMAGE itself -- caboose's
// own default base included, which is a separate image.
func TestCheckImages(t *testing.T) {
	for _, tc := range []struct {
		image, base string
		ok          bool
	}{
		{"caboose", "", true},
		{"caboose", "caboose-base", true},
		{"caboose", "node:22", true},
		{"caboose", "caboose", false},
		{"caboose", "caboose:latest", false},
		{"caboose:latest", "docker.io/library/caboose", false},
	} {
		c := &Config{Image: tc.image, BaseImage: tc.base}
		if err := c.CheckImages(); (err == nil) != tc.ok {
			t.Errorf("%+v: %v", tc, err)
		} else if err != nil && !strings.Contains(err.Error(), "names the same image as CABOOSE_IMAGE") {
			t.Errorf("%+v: %v", tc, err)
		}
	}
}

// An environment's image/ dir is its base, found by Load; with
// CABOOSE_BASE_IMAGE as well, which to build on would be a guess.
func TestImageDir(t *testing.T) {
	fs := fakeFS{"/h/.caboose/envs/work": "dir", "/h/.caboose/envs/work/image": "dir", "/h/.caboose/envs/default/image": "file"}
	c, err := Load(envOf(map[string]string{"HOME": "/h"}), fs, "work")
	if err != nil || c.ImageDir != "/h/.caboose/envs/work/image" {
		t.Fatalf("ImageDir %q, %v", c.ImageDir, err)
	}
	if ref, byo := c.Base(); ref != "caboose-work-base" || byo {
		t.Errorf("Base = %q, %v", ref, byo)
	}
	if err := c.CheckImages(); err != nil {
		t.Error(err)
	}
	c.BaseImage = "node:22"
	if err := c.CheckImages(); !errors.Is(err, ErrTwoBases) || !strings.Contains(err.Error(), "remove base_image from /h/.caboose/envs/work/config.toml") {
		t.Errorf("both: %v", err)
	}
	// Not a directory: not an image dir.
	if c, _ := Load(envOf(map[string]string{"HOME": "/h"}), fs, "default"); c.ImageDir != "" {
		t.Errorf("default: ImageDir %q", c.ImageDir)
	}
}

// The isolation and the vm's size come from config.toml, and a CABOOSE_
// variable wins over it, as every setting's does.
func TestIsolationSettings(t *testing.T) {
	file := "isolation = \"gvisor\"\nvm_cpus = 4\nvm_memory = \"8G\"\n"
	fsys := fakeFS{"/h/.caboose/envs/default/config.toml": file}
	c, err := Load(envOf(map[string]string{"HOME": "/h"}), fsys, "")
	if err != nil {
		t.Fatal(err)
	}
	if c.Isolation != "gvisor" || c.VMCPUs != "4" || c.VMMemory != "8G" {
		t.Errorf("from the file: %q %q %q", c.Isolation, c.VMCPUs, c.VMMemory)
	}
	c, err = Load(envOf(map[string]string{"HOME": "/h", "CABOOSE_ISOLATION": "vm", "CABOOSE_VM_CPUS": "2", "CABOOSE_VM_MEMORY": "4096M"}), fsys, "")
	if err != nil {
		t.Fatal(err)
	}
	if c.Isolation != "vm" || c.VMCPUs != "2" || c.VMMemory != "4096M" {
		t.Errorf("from the variables: %q %q %q", c.Isolation, c.VMCPUs, c.VMMemory)
	}
}

// The outbound proxy's settings: on by default, from the file, a variable
// winning over it; egress_proxy = false is off, never unset.
func TestEgressSettings(t *testing.T) {
	home := map[string]string{"HOME": "/h"}
	c, err := Load(envOf(home), fakeFS{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if c.EgressProxy != "on" || c.EgressPorts != "22 80 443" || c.EgressAllow != "" {
		t.Errorf("defaults: %q %q %q", c.EgressProxy, c.EgressPorts, c.EgressAllow)
	}
	file := "egress_proxy = \"off\"\negress_ports = \"443 8443\"\negress_allow = \"*.corp.example 10.0.0.0/8\"\n"
	fsys := fakeFS{cfgPath: file}
	if c, err = Load(envOf(home), fsys, ""); err != nil {
		t.Fatal(err)
	}
	if c.EgressProxy != "off" || c.EgressPorts != "443 8443" || c.EgressAllow != "*.corp.example 10.0.0.0/8" {
		t.Errorf("from the file: %q %q %q", c.EgressProxy, c.EgressPorts, c.EgressAllow)
	}
	c, err = Load(envOf(map[string]string{"HOME": "/h", "CABOOSE_EGRESS_PROXY": "on", "CABOOSE_EGRESS_PORTS": "22",
		"CABOOSE_EGRESS_ALLOW": "git.corp.example"}), fsys, "")
	if err != nil {
		t.Fatal(err)
	}
	if c.EgressProxy != "on" || c.EgressPorts != "22" || c.EgressAllow != "git.corp.example" {
		t.Errorf("from the variables: %q %q %q", c.EgressProxy, c.EgressPorts, c.EgressAllow)
	}
	for v, want := range map[string]string{"false": "off", "true": "on"} {
		c, err := Load(envOf(home), fakeFS{cfgPath: "egress_proxy = " + v + "\n"}, "")
		if err != nil || c.EgressProxy != want {
			t.Errorf("egress_proxy = %s: %q %v", v, c.EgressProxy, err)
		}
	}
	for v, want := range map[string]bool{"on": true, "off": false} {
		if on, err := CheckEgressProxy(v); err != nil || on != want {
			t.Errorf("CheckEgressProxy(%q) = %v %v", v, on, err)
		}
	}
	for _, bad := range []string{"", "yes", "1", "ON"} {
		if _, err := CheckEgressProxy(bad); err == nil {
			t.Errorf("CheckEgressProxy(%q) accepted", bad)
		}
	}
}

// ssh_agent names the host's agent for the sandbox: the file's, with ~
// expanded, the variable winning, and each saying where it came from.
func TestSSHAgentSetting(t *testing.T) {
	home := map[string]string{"HOME": "/h"}
	c, err := Load(envOf(home), fakeFS{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if c.SSHAgent != "" || c.SSHAgentFrom != "" {
		t.Errorf("default: %q from %q", c.SSHAgent, c.SSHAgentFrom)
	}
	fsys := fakeFS{cfgPath: "ssh_agent = \"~/op/agent.sock\"\n"}
	if c, err = Load(envOf(home), fsys, ""); err != nil {
		t.Fatal(err)
	}
	if c.SSHAgent != "/h/op/agent.sock" || c.SSHAgentFrom != cfgPath {
		t.Errorf("from the file: %q from %q", c.SSHAgent, c.SSHAgentFrom)
	}
	c, err = Load(envOf(map[string]string{"HOME": "/h", "CABOOSE_SSH_AGENT": "none"}), fsys, "")
	if err != nil {
		t.Fatal(err)
	}
	if c.SSHAgent != "none" || c.SSHAgentFrom != "CABOOSE_SSH_AGENT" {
		t.Errorf("from the variable: %q from %q", c.SSHAgent, c.SSHAgentFrom)
	}
}

// host_exec: off by default, from the file, a variable winning over it --
// one saying "0" or "false" is off, never set-and-so-on.
func TestHostExecSetting(t *testing.T) {
	home := map[string]string{"HOME": "/h"}
	c, err := Load(envOf(home), fakeFS{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if on, err := CheckHostExec(c.HostExec); on || err != nil || c.HostExecFrom != "" {
		t.Errorf("default: %q %v %v", c.HostExec, on, err)
	}
	fsys := fakeFS{cfgPath: "host_exec = true\n"}
	if c, err = Load(envOf(home), fsys, ""); err != nil {
		t.Fatal(err)
	}
	if on, _ := CheckHostExec(c.HostExec); !on || c.HostExecFrom != cfgPath {
		t.Errorf("from the file: %q from %q", c.HostExec, c.HostExecFrom)
	}
	for _, v := range []string{"0", "false", "off"} {
		c, err := Load(envOf(map[string]string{"HOME": "/h", "CABOOSE_HOST_EXEC": v}), fsys, "")
		if err != nil {
			t.Fatal(err)
		}
		if on, err := CheckHostExec(c.HostExec); on || err != nil || c.HostExecFrom != "CABOOSE_HOST_EXEC" {
			t.Errorf("CABOOSE_HOST_EXEC=%s over the file's true: %q %v %v", v, c.HostExec, on, err)
		}
	}
	c, err = Load(envOf(map[string]string{"HOME": "/h", "CABOOSE_HOST_EXEC": "1"}), fakeFS{cfgPath: "host_exec = false\n"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if on, _ := CheckHostExec(c.HostExec); !on {
		t.Errorf("CABOOSE_HOST_EXEC=1 over the file's false: %q", c.HostExec)
	}
	if c, err = Load(envOf(home), fakeFS{cfgPath: "host_exec = false\n"}, ""); err != nil || c.HostExec != "off" {
		t.Errorf("host_exec = false: %q %v", c.HostExec, err)
	}
	for v, want := range map[string]bool{"on": true, "true": true, "1": true, "yes": true, "TRUE": true, "": false, "off": false, "false": false, "0": false, "no": false} {
		if on, err := CheckHostExec(v); err != nil || on != want {
			t.Errorf("CheckHostExec(%q) = %v %v", v, on, err)
		}
	}
	for _, bad := range []string{"2", "maybe", "onn"} {
		if _, err := CheckHostExec(bad); err == nil || !strings.Contains(err.Error(), "host_exec") {
			t.Errorf("CheckHostExec(%q): %v", bad, err)
		}
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
