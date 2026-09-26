package caboose

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bfreis/caboose/internal/shelltest"
)

// These run the real layer-user.sh against a fake image root (its third
// argument, which prefixes every path it touches), with the shells images
// have: /bin/sh always, dash and BusyBox where installed, and bash --posix,
// which is /bin/sh on some images -- and sets UID itself, which is why the
// script takes the IDs as arguments. chown is a stub that logs; mkdir is
// the host's.

type layerImage struct {
	t         *testing.T
	root, bin string
	chownLog  string
}

// newLayerImage is an image root holding files (path relative to the root
// -> content), with bash at /bin/bash unless files says otherwise.
func newLayerImage(t *testing.T, files map[string]string) *layerImage {
	t.Helper()
	im := &layerImage{t: t, root: t.TempDir(), bin: t.TempDir()}
	im.chownLog = filepath.Join(im.bin, "chown.log")
	if _, ok := files["bin/bash"]; !ok {
		if _, ok := files["usr/bin/bash"]; !ok {
			files["bin/bash"] = ""
		}
	}
	for rel, content := range files {
		p := filepath.Join(im.root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	stub := "#!/bin/sh\necho \"$*\" >> '" + im.chownLog + "'\n"
	if err := os.WriteFile(filepath.Join(im.bin, "chown"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	mkdir, err := exec.LookPath("mkdir")
	if err != nil {
		t.Skip("no mkdir")
	}
	if err := os.Symlink(mkdir, filepath.Join(im.bin, "mkdir")); err != nil {
		t.Fatal(err)
	}
	return im
}

// run runs the script with shell, PATH = the stubs then /usr/bin (inside
// the image root, for finding bash; the host's, for nothing: every command
// the script runs is a stub or mkdir, found first).
func (im *layerImage) run(shell []string, uid, gid string) (string, error) {
	im.t.Helper()
	args := append(append([]string{}, shell[1:]...), "layer-user.sh", uid, gid, im.root)
	cmd := exec.Command(shell[0], args...)
	cmd.Env = []string{"PATH=" + im.bin + ":/usr/bin"}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (im *layerImage) read(rel string) string {
	im.t.Helper()
	b, err := os.ReadFile(filepath.Join(im.root, rel))
	if err != nil {
		if os.IsNotExist(err) {
			return "<absent>"
		}
		im.t.Fatal(err)
	}
	return string(b)
}

const (
	rootPasswd  = "root:x:0:0:root:/root:/bin/bash\n"
	rootGroup   = "root:x:0:\n"
	rootShadow  = "root:*:19000:0:99999:7:::\n"
	rootGshadow = "root:*::\n"
	dialout     = "dialout:x:20:\n"
)

func TestLayerUser(t *testing.T) {
	type files = map[string]string
	cases := []struct {
		name     string
		uid, gid string
		in       files
		want     files // "<absent>" for a file that must not exist
	}{
		{"fresh", "1000", "1000",
			files{"etc/passwd": rootPasswd, "etc/group": rootGroup, "etc/shadow": rootShadow, "etc/gshadow": rootGshadow},
			files{
				"etc/passwd":  rootPasswd + "agent:x:1000:1000::/home/agent:/bin/bash\n",
				"etc/group":   rootGroup + "agent:x:1000:\n",
				"etc/shadow":  rootShadow + "agent:!:::::::\n",
				"etc/gshadow": rootGshadow + "agent:!::\n",
			}},
		// ubuntu:24.04 and later ship a user and group ubuntu at 1000: the
		// user is taken over, the group reused as it is.
		{"uid held by ubuntu", "1000", "1000",
			files{
				"etc/passwd":  rootPasswd + "ubuntu:x:1000:1000:Ubuntu:/home/ubuntu:/bin/bash\n",
				"etc/group":   rootGroup + "adm:x:4:syslog,ubuntu\nubuntu:x:1000:\n",
				"etc/shadow":  rootShadow + "ubuntu:!:19000:0:99999:7:::\n",
				"etc/gshadow": rootGshadow + "ubuntu:!::\n",
			},
			files{
				"etc/passwd":  rootPasswd + "agent:x:1000:1000:Ubuntu:/home/agent:/bin/bash\n",
				"etc/group":   rootGroup + "adm:x:4:syslog,ubuntu\nubuntu:x:1000:\n",
				"etc/shadow":  rootShadow + "agent:!:19000:0:99999:7:::\n",
				"etc/gshadow": rootGshadow + "ubuntu:!::\n",
			}},
		// node:* images, on a macOS host: UID 501 free... but here node
		// holds the UID, and GID 20 is dialout, which stays dialout.
		{"uid held by node, gid by dialout", "1000", "20",
			files{
				"etc/passwd": rootPasswd + "node:x:1000:1000::/home/node:/bin/sh\n",
				"etc/group":  rootGroup + dialout + "node:x:1000:\n",
				"etc/shadow": rootShadow + "node:!:19000:0:99999:7:::\n",
			},
			files{
				"etc/passwd":  rootPasswd + "agent:x:1000:20::/home/agent:/bin/bash\n",
				"etc/group":   rootGroup + dialout + "node:x:1000:\n",
				"etc/shadow":  rootShadow + "agent:!:19000:0:99999:7:::\n",
				"etc/gshadow": "<absent>",
			}},
		// An agent user and group the image made for itself, at other IDs:
		// dropped, so the names stay unique, and ours added.
		{"a different agent", "1000", "1000",
			files{
				"etc/passwd":  rootPasswd + "agent:x:1001:1001::/home/agent:/bin/sh\nwww:x:33:33::/var/www:/usr/sbin/nologin\n",
				"etc/group":   rootGroup + "agent:x:1001:\nwww:x:33:agent\n",
				"etc/shadow":  rootShadow + "agent:$6$hash:19000:0:99999:7:::\n",
				"etc/gshadow": rootGshadow + "agent:!::\n",
			},
			files{
				"etc/passwd":  rootPasswd + "www:x:33:33::/var/www:/usr/sbin/nologin\nagent:x:1000:1000::/home/agent:/bin/bash\n",
				"etc/group":   rootGroup + "www:x:33:agent\nagent:x:1000:\n",
				"etc/shadow":  rootShadow + "agent:!:::::::\n",
				"etc/gshadow": rootGshadow + "agent:!::\n",
			}},
		// Both at once: ubuntu holds the UID and an agent exists elsewhere.
		{"uid held, and a different agent", "1000", "1000",
			files{
				"etc/passwd": rootPasswd + "agent:x:1001:1001::/home/agent:/bin/sh\nubuntu:x:1000:1000:Ubuntu:/home/ubuntu:/bin/bash\n",
				"etc/group":  rootGroup + "agent:x:1001:\nubuntu:x:1000:\n",
				"etc/shadow": rootShadow + "agent:!:1::::::\nubuntu:!:2::::::\n",
			},
			// The image's agent group, at 1001, is nobody's now: dropped.
			files{
				"etc/passwd": rootPasswd + "agent:x:1000:1000:Ubuntu:/home/agent:/bin/bash\n",
				"etc/group":  rootGroup + "ubuntu:x:1000:\n",
				"etc/shadow": rootShadow + "agent:!:2::::::\n",
			}},
		// A leftover agent group that something still uses stays: one with
		// members, or some user's primary group.
		{"agent group with members", "1000", "20",
			files{
				"etc/passwd":  rootPasswd,
				"etc/group":   rootGroup + dialout + "agent:x:1001:www\n",
				"etc/gshadow": rootGshadow + "agent:!::www\n",
			},
			files{
				"etc/passwd":  rootPasswd + "agent:!:1000:20::/home/agent:/bin/bash\n",
				"etc/group":   rootGroup + dialout + "agent:x:1001:www\n",
				"etc/gshadow": rootGshadow + "agent:!::www\n",
			}},
		{"agent group someone's primary", "1000", "20",
			files{
				"etc/passwd": rootPasswd + "svc:x:999:1001::/:/usr/sbin/nologin\n",
				"etc/group":  rootGroup + dialout + "agent:x:1001:\n",
			},
			files{
				"etc/passwd": rootPasswd + "svc:x:999:1001::/:/usr/sbin/nologin\nagent:!:1000:20::/home/agent:/bin/bash\n",
				"etc/group":  rootGroup + dialout + "agent:x:1001:\n",
			}},
		{"unused agent group, gid held", "1000", "20",
			files{
				"etc/passwd":  rootPasswd,
				"etc/group":   rootGroup + "agent:x:1001:\n" + dialout,
				"etc/gshadow": rootGshadow + "agent:!::\n",
			},
			files{
				"etc/group":   rootGroup + dialout,
				"etc/gshadow": rootGshadow,
			}},
		// Already right: nothing changes.
		{"agent already correct", "1000", "1000",
			files{
				"etc/passwd":  rootPasswd + "agent:x:1000:1000:caboose:/home/agent:/bin/bash\n",
				"etc/group":   rootGroup + "agent:x:1000:\n",
				"etc/shadow":  rootShadow + "agent:!:19000:0:99999:7:::\n",
				"etc/gshadow": rootGshadow + "agent:!::\n",
			},
			files{
				"etc/passwd":  rootPasswd + "agent:x:1000:1000:caboose:/home/agent:/bin/bash\n",
				"etc/group":   rootGroup + "agent:x:1000:\n",
				"etc/shadow":  rootShadow + "agent:!:19000:0:99999:7:::\n",
				"etc/gshadow": rootGshadow + "agent:!::\n",
			}},
		// BusyBox images may have no shadow at all: none is made, and the
		// new entry is locked in passwd itself.
		{"no shadow", "1000", "1000",
			files{"etc/passwd": "root:x:0:0:root:/root:/bin/ash\n", "etc/group": rootGroup},
			files{
				"etc/passwd": "root:x:0:0:root:/root:/bin/ash\nagent:!:1000:1000::/home/agent:/bin/bash\n",
				"etc/group":  rootGroup + "agent:x:1000:\n",
				"etc/shadow": "<absent>",
			}},
		{"no trailing newline", "1000", "1000",
			files{
				"etc/passwd": rootPasswd + "ubuntu:x:1000:1000::/home/ubuntu:/bin/bash",
				"etc/group":  "root:x:0:",
				"etc/shadow": "root:*:19000:0:99999:7:::",
			},
			files{
				"etc/passwd": rootPasswd + "agent:x:1000:1000::/home/agent:/bin/bash\n",
				"etc/group":  rootGroup + "agent:x:1000:\n",
				"etc/shadow": rootShadow + "agent:!:::::::\n",
			}},
		// Blank lines, comments and NIS lines are kept as they are, and a
		// new entry goes in before the first NIS line, not after it, where
		// NIS would answer for the name first.
		{"odd lines", "1000", "1000",
			files{
				"etc/passwd": rootPasswd + "\n# comment\n+::::::\n",
				"etc/group":  rootGroup + "\n+:::\n",
			},
			files{
				"etc/passwd": rootPasswd + "\n# comment\nagent:!:1000:1000::/home/agent:/bin/bash\n+::::::\n",
				"etc/group":  rootGroup + "\nagent:x:1000:\n+:::\n",
			}},
		{"NIS lines everywhere", "1000", "1000",
			files{
				"etc/passwd":  rootPasswd + "-baduser::::::\n+::::::\nlocal:x:1500:1500::/home/local:/bin/sh\n",
				"etc/group":   rootGroup + "+:::\n",
				"etc/shadow":  rootShadow + "+::::::::\n",
				"etc/gshadow": rootGshadow + "+:::\n",
			},
			files{
				"etc/passwd":  rootPasswd + "agent:x:1000:1000::/home/agent:/bin/bash\n-baduser::::::\n+::::::\nlocal:x:1500:1500::/home/local:/bin/sh\n",
				"etc/group":   rootGroup + "agent:x:1000:\n+:::\n",
				"etc/shadow":  rootShadow + "agent:!:::::::\n+::::::::\n",
				"etc/gshadow": rootGshadow + "agent:!::\n+:::\n",
			}},
		// No /bin/bash: the one on PATH, inside the image.
		{"bash elsewhere", "1000", "1000",
			files{"etc/passwd": rootPasswd, "etc/group": rootGroup, "usr/bin/bash": ""},
			files{"etc/passwd": rootPasswd + "agent:!:1000:1000::/home/agent:/usr/bin/bash\n"}},
	}
	for _, sh := range shelltest.Shells(t) {
		for _, tc := range cases {
			t.Run(filepath.Base(sh[0])+"/"+tc.name, func(t *testing.T) {
				im := newLayerImage(t, tc.in)
				if out, err := im.run(sh, tc.uid, tc.gid); err != nil {
					t.Fatalf("%v: %s", err, out)
				}
				got := map[string]string{}
				for rel, want := range tc.want {
					got[rel] = im.read(rel)
					if got[rel] != want {
						t.Errorf("%s:\n%s\nwant:\n%s", rel, got[rel], want)
					}
				}
				for _, d := range []string{".claude", ".local/bin", ".local/share/claude", ".local/state",
					".cache/claude", ".config/git", ".config/jj", ".config/gh"} {
					if fi, err := os.Stat(filepath.Join(im.root, "home/agent", d)); err != nil || !fi.IsDir() {
						t.Errorf("no ~/%s: %v", d, err)
					}
				}
				if log, _ := os.ReadFile(im.chownLog); string(log) != "-R "+tc.uid+":"+tc.gid+" "+im.root+"/home/agent\n" {
					t.Errorf("chown %q", log)
				}
				// Running it again changes nothing.
				if out, err := im.run(sh, tc.uid, tc.gid); err != nil {
					t.Fatalf("second run: %v: %s", err, out)
				}
				for rel := range tc.want {
					if again := im.read(rel); again != got[rel] {
						t.Errorf("second run changed %s:\n%s\nwas:\n%s", rel, again, got[rel])
					}
				}
			})
		}
	}
}

func TestLayerUserRefuses(t *testing.T) {
	for _, tc := range []struct {
		name, uid, gid string
		in             map[string]string
		want           string
	}{
		{"uid not a number", "10x", "1000", map[string]string{"etc/passwd": rootPasswd, "etc/group": rootGroup}, "UID must be a number"},
		{"no gid", "1000", "", map[string]string{"etc/passwd": rootPasswd, "etc/group": rootGroup}, "GID must be a number"},
		{"no passwd", "1000", "1000", map[string]string{"etc/group": rootGroup}, "no /etc/passwd"},
		// A host root would take over the image's root user.
		{"uid 0", "0", "1000", map[string]string{"etc/passwd": rootPasswd, "etc/group": rootGroup}, "UID 0: caboose must not run as root"},
		{"uid 00", "00", "1000", map[string]string{"etc/passwd": rootPasswd, "etc/group": rootGroup}, "UID 0: caboose must not run as root"},
		{"gid 0", "1000", "0", map[string]string{"etc/passwd": rootPasswd, "etc/group": rootGroup}, "GID 0: caboose must not run with group root"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			im := newLayerImage(t, tc.in)
			out, err := im.run([]string{"/bin/sh"}, tc.uid, tc.gid)
			if err == nil || !strings.Contains(out, "caboose layer: "+tc.want) {
				t.Errorf("err %v, output %q", err, out)
			}
			if _, err := os.Stat(filepath.Join(im.root, "home")); !os.IsNotExist(err) {
				t.Error("a refused run created the home")
			}
		})
	}
	// A symlinked /home/agent: refused before anything is edited.
	im0 := newLayerImage(t, map[string]string{"etc/passwd": rootPasswd, "etc/group": rootGroup, "srv/agent/.keep": ""})
	if err := os.MkdirAll(filepath.Join(im0.root, "home"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../srv/agent", filepath.Join(im0.root, "home/agent")); err != nil {
		t.Fatal(err)
	}
	if out, err := im0.run([]string{"/bin/sh"}, "1000", "1000"); err == nil || !strings.Contains(out, "caboose layer: /home/agent is a symlink") {
		t.Errorf("err %v, output %q", err, out)
	}
	if im0.read("etc/passwd") != rootPasswd {
		t.Error("edited passwd before refusing")
	}

	// No bash anywhere on PATH in the image.
	im := newLayerImage(t, map[string]string{"etc/passwd": rootPasswd, "etc/group": rootGroup, "usr/bin/bash": ""})
	if err := os.Remove(filepath.Join(im.root, "usr/bin/bash")); err != nil {
		t.Fatal(err)
	}
	if out, err := im.run([]string{"/bin/sh"}, "1000", "1000"); err == nil || !strings.Contains(out, "no bash in the image") {
		t.Errorf("err %v, output %q", err, out)
	}
}
