package config

import (
	"reflect"
	"strings"
	"testing"
)

const cfgPath = "/h/.caboose/envs/default/config.toml"

func TestConfigFile(t *testing.T) {
	fs := fakeFS{cfgPath: `
repo_root = "~/src"
base_image = "debian:13.7-slim"
keep_versions = 3
no_tmux = true
no_auto_build = false
docker_sock = "/var/run/docker.sock"
`}
	c, err := Load(envOf(map[string]string{"HOME": "/h"}), fs, "")
	if err != nil {
		t.Fatal(err)
	}
	if c.Roots[0].Host != "/h/src" || c.BaseImage != "debian:13.7-slim" || c.KeepVersions != "3" ||
		c.NoTmux != "1" || c.NoAutoBuild != "" || c.DockerSock != "/var/run/docker.sock" {
		t.Errorf("config %+v", *c)
	}
	if got := c.RootsOrigin(); got != "set by repo_root in "+cfgPath {
		t.Errorf("origin %q", got)
	}

	// A variable wins over the file, for one shell.
	c, _ = Load(envOf(map[string]string{"HOME": "/h", "CABOOSE_REPO_ROOT": "/r", "CABOOSE_KEEP_VERSIONS": "5"}), fs, "")
	if c.Roots[0].Host != "/r" || c.KeepVersions != "5" || c.RootsOrigin() != "set by CABOOSE_REPO_ROOT" {
		t.Errorf("repo root %q keep %q origin %q", c.Roots[0].Host, c.KeepVersions, c.RootsOrigin())
	}
}

// Each environment reads its own file, and no other.
func TestConfigFilePerEnv(t *testing.T) {
	fs := fakeFS{cfgPath: `repo_root = "/a"`, "/h/.caboose/envs/work/config.toml": `repo_root = "/b"`}
	for env, want := range map[string]string{"": "/a", "work": "/b", "play": "/h/dev"} {
		c, err := Load(envOf(map[string]string{"HOME": "/h"}), fs, env)
		if err != nil || c.Roots[0].Host != want {
			t.Errorf("env %q: repo root %+v, %v", env, c.Roots, err)
		}
	}
}

func TestConfigFileRefuses(t *testing.T) {
	for name, tc := range map[string]struct{ body, err string }{
		"a misspelled key":    {`repo_rot = "/x"`, "unknown setting repo_rot (known: auto_sync, base_image, container,"},
		"another table":       {"[mounts]\ndev = \"/x\"", "unknown setting mounts"},
		"roots not a table":   {`roots = "/x"`, "roots must be a table"},
		"empty roots":         {"[roots]", "[roots] names no roots"},
		"a bad root name":     {"[roots]\n\"My Dev\" = \"/x\"", "'My Dev' is not a root name"},
		"a root not a path":   {"[roots]\ndev = 1", "roots.dev must be a path"},
		"roots and repo_root": {"repo_root = \"/r\"\n[roots]\ndev = \"/x\"", "repo_root and [roots] both"},
		"a list":              {`repo_root = ["/x"]`, "repo_root must be a string, a number or true/false"},
		"not TOML":            {`repo_root = `, cfgPath + ":"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Load(envOf(map[string]string{"HOME": "/h"}), fakeFS{cfgPath: tc.body}, "")
			if err == nil || !strings.Contains(err.Error(), tc.err) || !strings.Contains(err.Error(), cfgPath) {
				t.Errorf("err = %v, want it to name %q", err, tc.err)
			}
		})
	}
}

// The template is valid, and every setting in it is one the file accepts.
func TestTemplate(t *testing.T) {
	uncommented := strings.NewReplacer("\n#repo_root", "\nrepo_root", "\n#base_image", "\nbase_image",
		"\n#keep_versions", "\nkeep_versions", "\n#docker_sock", "\ndocker_sock",
		"\n#no_tmux", "\nno_tmux", "\n#no_auto_build", "\nno_auto_build", "\n#auto_sync", "\nauto_sync").Replace(Template)
	c, err := Load(envOf(map[string]string{"HOME": "/h"}), fakeFS{cfgPath: uncommented}, "")
	if err != nil {
		t.Fatal(err)
	}
	if c.Roots[0].Host != "/h/dev" || c.KeepVersions != "2" || c.NoTmux != "1" || c.AutoSync != "1" {
		t.Errorf("config %+v", *c)
	}
	if _, err := Load(envOf(map[string]string{"HOME": "/h"}), fakeFS{cfgPath: Template}, ""); err != nil {
		t.Errorf("as written: %v", err)
	}
}

// [roots] mounts each root at /work/<name>, in name order; a ~ in one is
// the home dir. CABOOSE_REPO_ROOT still wins, as a single root at /work.
func TestConfigFileRoots(t *testing.T) {
	fs := fakeFS{cfgPath: "[roots]\nwork = \"/w\"\ndev = \"~/dev\"\n"}
	c, err := Load(envOf(map[string]string{"HOME": "/h"}), fs, "")
	if err != nil {
		t.Fatal(err)
	}
	want := []Root{{Name: "dev", Host: "/h/dev", Container: "/work/dev"}, {Name: "work", Host: "/w", Container: "/work/work"}}
	if !reflect.DeepEqual(c.Roots, want) || c.RootsOrigin() != "set by [roots] in "+cfgPath {
		t.Errorf("roots %+v, origin %q", c.Roots, c.RootsOrigin())
	}
	c, err = Load(envOf(map[string]string{"HOME": "/h", "CABOOSE_REPO_ROOT": "/r"}), fs, "")
	if err != nil || !reflect.DeepEqual(c.Roots, []Root{{Host: "/r", Container: "/work"}}) || c.RootsOrigin() != "set by CABOOSE_REPO_ROOT" {
		t.Errorf("with CABOOSE_REPO_ROOT: roots %+v, origin %q, %v", c.Roots, c.RootsOrigin(), err)
	}
	// Roots from the file need no HOME for a default.
	c, err = Load(envOf(map[string]string{"CABOOSE_HOME": "/c"}), fakeFS{"/c/envs/default/config.toml": "[roots]\na = \"/a\"\n"}, "")
	if err != nil || c.Roots[0].Host != "/a" {
		t.Errorf("no HOME, roots from the file: %+v, %v", c, err)
	}
}
