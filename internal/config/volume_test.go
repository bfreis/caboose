package config

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// A profile's volumes are its own [<kind>.NAME.volume.VOLUME] tables:
// under vm a disk of size (64G when it says none), under container and
// gvisor a path alone. Only the profile in use gives the Config any.
func TestVolumes(t *testing.T) {
	c, err := load(t, `
isolation = "vm.a"

[vm.a]
cpus = 2

[vm.a.volume.cache]
path = "~/.cache/go-build"
size = "40G"

[vm.a.volume.data]
path = "/data"

[vm.a.volume.go]
path = "/home/agent/go"

[vm.a.volume.a2345678901234567890]
path = "/twenty"

[container.b]
[container.b.volume.cache]
path = "~/.cache/pip"
`)
	if err != nil {
		t.Fatal(err)
	}
	want := []Volume{
		{Name: "a2345678901234567890", Path: "/twenty", Size: DefaultVolumeSize},
		{Name: "cache", Path: "/home/agent/.cache/go-build", Size: 40 << 30},
		{Name: "data", Path: "/data", Size: DefaultVolumeSize},
		{Name: "go", Path: "/home/agent/go", Size: DefaultVolumeSize},
	}
	if !reflect.DeepEqual(c.Volumes, want) {
		t.Errorf("vm volumes %+v", c.Volumes)
	}
	if got := c.Origin("vm.a.volume.cache.size"); got != "size in [vm.a.volume.cache] of "+cfgPath {
		t.Errorf("origin %q", got)
	}
	// Under container, a docker volume: no size.
	c, err = load(t, "[container.b]\n[container.b.volume.cache]\npath = \"~/.cache/pip\"\n")
	if err != nil || !reflect.DeepEqual(c.Volumes, []Volume{{Name: "cache", Path: "/home/agent/.cache/pip"}}) {
		t.Errorf("container volumes %+v %v", c.Volumes, err)
	}
	// No inheritance: the profile not in use gives none.
	c, err = load(t, "isolation = \"vm.a\"\n[vm.a]\n[vm.b]\n[vm.b.volume.cache]\npath = \"/c\"\n")
	if err != nil || c.Volumes != nil {
		t.Errorf("unused profile's volumes %+v %v", c.Volumes, err)
	}
	// A path beside the roots, and roots of their own paths.
	c, err = load(t, "[roots]\ntools = { host = \"/t\", path = \"/opt/tools\" }\n[vm.a]\n[vm.a.volume.w]\npath = \"/opt/work\"\n")
	if err != nil || len(c.Volumes) != 1 || c.Volumes[0].Path != "/opt/work" {
		t.Errorf("beside a root %+v %v", c.Volumes, err)
	}
	// The most a VM takes.
	var b strings.Builder
	b.WriteString("[vm.a]\n")
	for i := range MaxVMVolumes {
		fmt.Fprintf(&b, "[vm.a.volume.v%d]\npath = \"/v/%d\"\n", i, i)
	}
	if c, err := load(t, b.String()); err != nil || len(c.Volumes) != MaxVMVolumes {
		t.Errorf("%d volumes: %d, %v", MaxVMVolumes, len(c.Volumes), err)
	}
}

// What a volume cannot be is refused when the file is read, in any
// profile, used or not.
func TestVolumeRefusals(t *testing.T) {
	vol := func(path string) string { return "[vm.a]\n[vm.a.volume.v]\npath = \"" + path + "\"\n" }
	inPath := "path in [vm.a.volume.v]: "
	var many strings.Builder
	many.WriteString("[vm.a]\n")
	for i := range MaxVMVolumes + 1 {
		fmt.Fprintf(&many, "[vm.a.volume.v%d]\npath = \"/v/%d\"\n", i, i)
	}
	for name, tc := range map[string]struct{ body, err string }{
		// Structure: what a kind takes.
		"a size under container": {"[container.a]\n[container.a.volume.v]\npath = \"/v\"\nsize = \"8G\"", "unknown setting in [container.a.volume.v] size (known: path;"},
		"a size under gvisor":    {"[gvisor.a]\n[gvisor.a.volume.v]\npath = \"/v\"\nsize = \"8G\"", "unknown setting in [gvisor.a.volume.v] size (known: path;"},
		"an unknown key":         {"[vm.a]\n[vm.a.volume.v]\npath = \"/v\"\nmode = \"rw\"", "unknown setting in [vm.a.volume.v] mode (known: path, size;"},
		"no path":                {"[vm.a]\n[vm.a.volume.v]\nsize = \"8G\"", "[vm.a.volume.v] needs path, where the sandbox sees the volume"},
		"a path not a string":    {"[vm.a]\n[vm.a.volume.v]\npath = 1", "path in [vm.a.volume.v] must be a string"},
		"volume not tables":      {"[vm.a]\nvolume = \"/v\"", "volume in [vm.a] must be tables of volumes, [vm.a.volume.NAME]"},
		"a volume not a table":   {"[vm.a]\n[vm.a.volume]\nv = \"/v\"", "volume in [vm.a] must be tables of volumes, [vm.a.volume.NAME]"},
		"under an image profile": {"[apko.a]\n[apko.a.volume.v]\npath = \"/v\"", "unknown setting in [apko.a] volume"},
		"in no profile":          {"[vm.a.volume.v]\npath = \"/v\"", "[vm.a.volume.v] is in [vm.a], which the file does not define: add a [vm.a] table above it"},
		"in an unused profile":   {"isolation = \"vm.a\"\n[vm.a]\n[vm.b]\n[vm.b.volume.v]\npath = \"/v\"\nsize = \"1G\"", "size in [vm.b.volume.v]: \"1G\" is outside 4G to 1024G"},
		"a profile lists them":   {"[vm.a]\nrun_args = []", "unknown setting in [vm.a] run_args (known: [vm.a.volume.NAME], cpus,"},

		// Names: a file name and a docker volume's.
		"an uppercase name":    {"[vm.a]\n[vm.a.volume.Cache]\npath = \"/v\"", "[vm.a.volume.Cache]: 'Cache' is not a volume name: lowercase letters, digits, - and _"},
		"a name with a slash":  {"[vm.a]\n[vm.a.volume.\"a/b\"]\npath = \"/v\"", "'a/b' is not a volume name"},
		"a name of dots":       {"[vm.a]\n[vm.a.volume.\"..\"]\npath = \"/v\"", "'..' is not a volume name"},
		"a name too long":      {"[vm.a]\n[vm.a.volume." + strings.Repeat("v", 21) + "]\npath = \"/v\"", "is not a volume name"},
		"caboose's docker":     {"[vm.a]\n[vm.a.volume.docker]\npath = \"/v\"", "[vm.a.volume.docker]: docker is the volume caboose keeps the sandbox's dockerd storage on"},
		"docker, any kind":     {"[container.a]\n[container.a.volume.docker]\npath = \"/v\"", "docker is the volume caboose keeps"},
		"more than a VM takes": {many.String(), fmt.Sprintf("[vm.a] has %d volumes, and a VM takes at most %d besides caboose's", MaxVMVolumes+1, MaxVMVolumes)},

		// Sizes: vm only, a disk's.
		"a size too small":    {"[vm.a]\n[vm.a.volume.v]\npath = \"/v\"\nsize = \"2G\"", "size in [vm.a.volume.v]: \"2G\" is outside 4G to 1024G"},
		"a size too big":      {"[vm.a]\n[vm.a.volume.v]\npath = \"/v\"\nsize = \"2048G\"", "size in [vm.a.volume.v]: \"2048G\" is outside 4G to 1024G"},
		"a size as a word":    {"[vm.a]\n[vm.a.volume.v]\npath = \"/v\"\nsize = \"big\"", "size in [vm.a.volume.v]: \"big\" is not an amount like"},
		"a size as a number":  {"[vm.a]\n[vm.a.volume.v]\npath = \"/v\"\nsize = 64", "size in [vm.a.volume.v] must be a string"},
		"a size of a profile": {"[vm.a]\nsize = \"8G\"", "unknown setting in [vm.a] size"},

		// Paths: caboose's mounts and what it reads from the host.
		"caboose's bin":          {vol("~/.local/bin"), inPath + "~/.local/bin is caboose's own"},
		"inside claude's share":  {vol("~/.local/share/claude/x"), inPath + "~/.local/share/claude/x is inside ~/.local/share/claude, which caboose mounts itself"},
		"holding caboose's":      {vol("~/.local"), inPath + "~/.local holds ~/.local/bin, which caboose mounts itself"},
		"the proposals":          {vol("~/.caboose-proposals"), inPath + "~/.caboose-proposals is caboose's own"},
		"claude's cache":         {vol("/home/agent/.cache/claude"), inPath + "~/.cache/claude is caboose's own"},
		"holding claude's cache": {vol("~/.cache"), inPath + "~/.cache holds ~/.cache/claude, which caboose mounts itself"},
		"holding the config":     {vol("~/.config"), inPath + "~/.config holds ~/.config/caboose, which is kept on this machine"},
		"the state":              {vol("~/.local/state"), inPath + "~/.local/state is caboose's own"},
		"~/.claude":              {vol("~/.claude"), inPath + "~/.claude is kept on this machine, which reads it: it cannot be a volume"},
		"inside ~/.claude":       {vol("~/.claude/projects"), inPath + "~/.claude/projects is inside ~/.claude, which is kept on this machine"},
		"~/.claude.json":         {vol("~/.claude.json"), inPath + "~/.claude.json is kept on this machine"},
		"caboose's config":       {vol("~/.config/caboose"), inPath + "~/.config/caboose is kept on this machine"},
		"the home":               {vol("~"), inPath + `"~" is the whole home`},
		"the home, spelled out":  {vol("/home/agent"), inPath + `"/home/agent" is the whole home`},
		"too deep in the home":   {vol("~/go/pkg/mod"), inPath + `"~/go/pkg/mod" is too deep: a volume in the home sits directly in ~, ~/.config, ~/.local, ~/.local/share or ~/.cache`},
		"an unclean home path":   {vol("~/a/../b"), inPath + `"~/a/../b" is not a clean path under the home`},
		"a colon in the home":    {vol("~/a:b"), inPath + `path "~/a:b" has ':'`},
		"another's home":         {vol("~bob/x"), inPath + `"~bob/x": only ~/ (the sandbox's home) may start a path`},

		// Paths: the system's.
		"relative":       {vol("cache"), inPath + `"cache" is not an absolute, clean path in the sandbox (or one starting with ~/)`},
		"unclean":        {vol("/data/../x"), inPath + `"/data/../x" is not an absolute, clean path`},
		"the root":       {vol("/"), inPath + `path "/" would cover the whole sandbox`},
		"docker's place": {vol("/var/lib/docker"), inPath + `path "/var/lib/docker" is at, inside or around /var`},
		"/usr":           {vol("/usr/local"), inPath + `path "/usr/local" is at, inside or around /usr`},
		"/etc":           {vol("/etc/claude-code"), inPath + `path "/etc/claude-code" is at, inside or around /etc`},
		"/tmp":           {vol("/tmp"), inPath + `path "/tmp" is at, inside or around /tmp`},
		"/lib64":         {vol("/lib64"), inPath + `path "/lib64" is in /lib64, which the sandbox's own system needs`},
		"/mnt itself":    {vol("/mnt"), inPath + `path "/mnt" would hide what the image keeps in /mnt`},
		"another home":   {vol("/home/bob"), inPath + `path "/home/bob" is at, inside or around /home`},
		"the agent sock": {vol("/ssh-agent.sock"), inPath + `path "/ssh-agent.sock" is at, inside or around /ssh-agent.sock`},
		"a colon":        {vol("/a:b"), inPath + `path "/a:b" has ':'`},

		// Paths: the roots'.
		"at a root":          {vol("/work/dev"), inPath + `"/work/dev" is at, inside or around root dev at /work/dev`},
		"inside a root":      {vol("/work/dev/node_modules"), inPath + `"/work/dev/node_modules" is at, inside or around root dev at /work/dev`},
		"around the roots":   {vol("/work"), inPath + `"/work" is at, inside or around root dev at /work/dev`},
		"beside a root":      {vol("/work/cache"), inPath + `"/work/cache" is at, inside or around /work, which is for the roots`},
		"inside a long root": {"[roots]\ntools = { host = \"/t\", path = \"/opt/tools\" }\n" + vol("/opt/tools/cache"), inPath + `"/opt/tools/cache" is at, inside or around root tools at /opt/tools`},
		"around a long root": {"[roots]\ntools = { host = \"/t\", path = \"/srv/a/tools\" }\n" + vol("/srv/a"), inPath + `"/srv/a" is at, inside or around root tools at /srv/a/tools`},

		// Volumes against each other.
		"nesting volumes":        {"[vm.a]\n[vm.a.volume.a]\npath = \"/data\"\n[vm.a.volume.b]\npath = \"/data/x\"", "volumes a and b of [vm.a] nest (/data, /data/x): give them paths that do not contain one another"},
		"nesting, the other way": {"[vm.a]\n[vm.a.volume.a]\npath = \"/data/x\"\n[vm.a.volume.b]\npath = \"/data\"", "volumes b and a of [vm.a] nest (/data, /data/x)"},
		"the same path":          {"[vm.a]\n[vm.a.volume.a]\npath = \"~/x\"\n[vm.a.volume.b]\npath = \"/home/agent/x\"", "volumes a and b of [vm.a] are both at /home/agent/x"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := load(t, tc.body)
			if err == nil || !strings.Contains(err.Error(), tc.err) || !strings.HasPrefix(err.Error(), cfgPath+": ") {
				t.Errorf("err %v, want %q", err, tc.err)
			}
		})
	}
}

// caboose volume create and resize are line edits of config.toml: the
// volume's table, made when missing, and its keys. The result reads back.
func TestVolumeEdit(t *testing.T) {
	orig := []byte(Template)
	e := Edit{
		Tables: []string{"vm.default", "vm.default.volume.cache"},
		Set:    map[string]any{"vm.default.volume.cache.path": "~/.cache/go-build", "vm.default.volume.cache.size": "40G"},
	}
	created := EditFile(orig, e)
	if err := CheckEdit(cfgPath, orig, created, e); err != nil {
		t.Fatalf("%v\n%s", err, created)
	}
	c, err := load(t, string(created))
	if err != nil || !reflect.DeepEqual(c.Volumes, []Volume{{Name: "cache", Path: "/home/agent/.cache/go-build", Size: 40 << 30}}) {
		t.Fatalf("created: %+v %v\n%s", c.Volumes, err, created)
	}
	// A volume table the edit did not make is not there.
	missing := Edit{Tables: []string{"vm.default.volume.other"}}
	if err := CheckEdit(cfgPath, created, created, missing); err == nil || err.Error() != "[vm.default.volume.other] is not there" {
		t.Errorf("a missing volume: %v", err)
	}
	r := Edit{Set: map[string]any{"vm.default.volume.cache.size": "80G"}}
	resized := EditFile(created, r)
	if err := CheckEdit(cfgPath, created, resized, r); err != nil {
		t.Fatal(err)
	}
	if c, err := load(t, string(resized)); err != nil || len(c.Volumes) != 1 || c.Volumes[0].Size != 80<<30 {
		t.Errorf("resized: %+v %v\n%s", c.Volumes, err, resized)
	}
	// caboose volume rm comments the table out; made again, it is the
	// file it was.
	rm := Edit{Remove: []string{"vm.default.volume.cache"}}
	removed := EditFile(resized, rm)
	if err := CheckEdit(cfgPath, resized, removed, rm); err != nil {
		t.Fatalf("%v\n%s", err, removed)
	}
	if c, err := load(t, string(removed)); err != nil || len(c.Volumes) != 0 {
		t.Errorf("removed: %+v %v\n%s", c.Volumes, err, removed)
	}
	again := Edit{Tables: []string{"vm.default.volume.cache"},
		Set: map[string]any{"vm.default.volume.cache.path": "~/.cache/go-build", "vm.default.volume.cache.size": "80G"}}
	if again := EditFile(removed, again); string(again) != string(resized) {
		t.Errorf("made again:\n%s\nwas:\n%s", again, resized)
	}
}
