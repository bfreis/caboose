package config

import (
	"reflect"
	"strings"
	"testing"
)

const cfgPath = "/h/.caboose/envs/default/config.toml"

func load(t *testing.T, body string) (*Config, error) {
	t.Helper()
	return Load(envOf(map[string]string{"HOME": "/h"}), fakeFS{cfgPath: body}, "")
}

func TestConfigFile(t *testing.T) {
	c, err := load(t, `
format = 1

[image]
base = "debian:13.7-slim"
auto_build = false

[session]
tmux = false
tz = "Europe/Lisbon"
hostname = "box"
auto_sync = true
keep_versions = 3
ready_timeout = 60

[link]
forward_ports = ["3000-3999", 5173]
open_urls = "allow"
ssh_agent = "~/agent.sock"
host_exec = true
`)
	if err != nil {
		t.Fatal(err)
	}
	if c.BaseImage != "debian:13.7-slim" || c.AutoBuild || c.Tmux || c.TZ != "Europe/Lisbon" || c.Hostname != "box" ||
		!c.AutoSync || c.KeepVersions != 3 || c.ReadyTimeout != 60 || c.ForwardPorts != "3000-3999 5173" ||
		c.OpenURLs != "allow" || c.SSHAgent != "/h/agent.sock" || !c.HostExec || c.File.Format != 1 {
		t.Errorf("config %+v", *c)
	}
	if c.Isolation != KindContainer || c.Profile != "" || c.RunArgs != nil || c.EngineSocket {
		t.Errorf("isolation %q %q %q %v", c.Isolation, c.Profile, c.RunArgs, c.EngineSocket)
	}
	if got := c.Origin("link.host_exec"); got != "host_exec in [link] of "+cfgPath {
		t.Errorf("origin %q", got)
	}
	if got := c.Origin("session.tz"); got != "tz in [session] of "+cfgPath {
		t.Errorf("origin %q", got)
	}
	// An empty array of ports is none.
	if c, err := load(t, "[link]\nforward_ports = []\n"); err != nil || c.ForwardPorts != "none" {
		t.Errorf("no ports: %q %v", c.ForwardPorts, err)
	}
}

// Each environment reads its own file, and no other.
func TestConfigFilePerEnv(t *testing.T) {
	fs := fakeFS{cfgPath: "[session]\ntz = \"A\"\n", "/h/.caboose/envs/work/config.toml": "[session]\ntz = \"B\"\n"}
	for env, want := range map[string]string{"": "A", "work": "B", "play": ""} {
		c, err := Load(envOf(map[string]string{"HOME": "/h"}), fs, env)
		if err != nil || c.TZ != want {
			t.Errorf("env %q: tz %q, %v", env, c.TZ, err)
		}
	}
}

// Each kind's profile takes its own keys.
func TestProfiles(t *testing.T) {
	c, err := load(t, "[container.default]\nrun_args = [\"--cap-add=NET_ADMIN\", \"--label=a=b c\"]\nengine_socket = true\n")
	if err != nil || c.Isolation != KindContainer || c.Profile != "container.default" ||
		!reflect.DeepEqual(c.RunArgs, []string{"--cap-add=NET_ADMIN", "--label=a=b c"}) || !c.EngineSocket {
		t.Errorf("container: %+v %v", c, err)
	}
	c, err = load(t, "[gvisor.strict]\n")
	if err != nil || c.Isolation != KindGVisor || c.Profile != "gvisor.strict" || c.RunArgs != nil {
		t.Errorf("gvisor: %+v %v", c, err)
	}
	c, err = load(t, "[vm.big]\ncpus = 8\nmemory = \"16G\"\negress = false\negress_ports = [22, \"8000-8100\"]\negress_allow = [\"*.corp.example\", \"10.0.0.0/8\"]\n")
	if err != nil || c.Isolation != KindVM || c.Profile != "vm.big" || c.VMCPUs != 8 || c.VMMemory != "16G" || c.Egress ||
		c.EgressPorts != "22 8000-8100" || !reflect.DeepEqual(c.EgressAllow, []string{"*.corp.example", "10.0.0.0/8"}) {
		t.Errorf("vm: %+v %v", c, err)
	}
	if got := c.ProfileKey("cpus"); got != "cpus in [vm.big]" {
		t.Errorf("ProfileKey %q", got)
	}
	// A vm profile's defaults.
	c, err = load(t, "[vm.default]\n")
	if err != nil || !c.Egress || c.EgressPorts != DefaultEgressPorts || c.VMCPUs != 0 || c.VMMemory != "" {
		t.Errorf("vm defaults: %+v %v", c, err)
	}
	// No inheritance: the profile not in use gives nothing.
	c, err = load(t, "isolation = \"container.a\"\n[container.a]\n[container.b]\nengine_socket = true\nrun_args = [\"--init\"]\n")
	if err != nil || c.Profile != "container.a" || c.EngineSocket || c.RunArgs != nil {
		t.Errorf("two profiles: %+v %v", c, err)
	}
}

// Which profile is used: the one isolation names, else the only one,
// else none at all is a plain container; several and no isolation, or
// isolation naming none of them, is an error.
func TestProfileSelection(t *testing.T) {
	for _, tc := range []struct{ name, body, profile, err string }{
		{"none", "", "", ""},
		{"one", "[vm.default]\n", "vm.default", ""},
		{"named", "isolation = \"gvisor.b\"\n[gvisor.a]\n[gvisor.b]\n[vm.c]\n", "gvisor.b", ""},
		{"several, unnamed", "[gvisor.a]\n[vm.c]\n", "", "defines the isolation profiles gvisor.a, vm.c, and isolation does not say which"},
		{"missing", "isolation = \"vm.big\"\n[vm.small]\n", "", `isolation = "vm.big" names a profile the file does not define (vm.small are): add a [vm.big] table`},
		{"missing, none defined", "isolation = \"vm.big\"\n", "", "(none is)"},
		{"a bare kind", "isolation = \"gvisor\"\n[gvisor.default]\n", "", `isolation = "gvisor" is not a profile, "<kind>.<name>"`},
		{"the old docker", "isolation = \"docker\"\n", "", `"docker" is the container kind now`},
		{"an unknown kind", "isolation = \"kvm.a\"\n", "", "is not a profile"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := load(t, tc.body)
			switch {
			case tc.err != "":
				if err == nil || !strings.Contains(err.Error(), tc.err) || !strings.HasPrefix(err.Error(), cfgPath+": ") {
					t.Errorf("err %v, want %q", err, tc.err)
				}
			case err != nil:
				t.Error(err)
			case c.Profile != tc.profile:
				t.Errorf("profile %q, want %q", c.Profile, tc.profile)
			}
		})
	}
}

func TestConfigFileRefuses(t *testing.T) {
	for name, tc := range map[string]struct{ body, err string }{
		"a misspelled key":       {`isolaton = "vm.a"`, "unknown top-level setting isolaton (known: [container.NAME], [gvisor.NAME], [image], [link], [roots], [session], [vm.NAME], format, isolation; see docs/configuration.md, or 'caboose update' if this caboose predates it)"},
		"a misspelled table key": {"[session]\ntmuz = true", "unknown setting in [session] tmuz (known: auto_sync, hostname, keep_versions, ready_timeout, tmux, tz;"},
		"another table":          {"[mounts]\ndev = \"/x\"", "unknown top-level setting mounts"},
		"a key of another kind":  {"[vm.a]\nrun_args = [\"--init\"]", "unknown setting in [vm.a] run_args (known: cpus, egress, egress_allow, egress_ports, memory;"},
		"vm keys under gvisor":   {"[gvisor.a]\ncpus = 2", "unknown setting in [gvisor.a] cpus"},
		"a table as a key":       {"session = 1", "session must be a table"},
		"a kind not of tables":   {"[vm]\ncpus = 1", "vm must be tables of profiles, [vm.NAME]"},
		"a bad profile name":     {"[vm.Big]", "'Big' is not a profile name"},
		"a string for a bool":    {"[session]\ntmux = \"false\"", "tmux in [session] must be true or false"},
		"a number for a bool":    {"[link]\nhost_exec = 1", "host_exec in [link] must be true or false"},
		"a bool for a string":    {"[image]\nbase = true", "base in [image] must be a string"},
		"a string for a number":  {"[session]\nkeep_versions = \"3\"", "keep_versions in [session] must be a whole number"},
		"a zero":                 {"[session]\nready_timeout = 0", "ready_timeout in [session] must be a whole number, 1 or more"},
		"no cpus":                {"[vm.a]\ncpus = 0", "cpus in [vm.a] must be a whole number, 1 or more"},
		"ports as a string":      {"[link]\nforward_ports = \"3000-3999\"", `forward_ports in [link] must be an array of ports and "a-b" ranges`},
		"a port out of range":    {"[link]\nforward_ports = [70000]", `"70000" is not a port`},
		"a port zero":            {"[link]\nforward_ports = [0]", `"0" is not a port`},
		"a range backwards":      {"[link]\nforward_ports = [\"9-1\"]", `"9-1" ends before it starts`},
		"a range out of range":   {"[vm.a]\negress_ports = [\"99999-100000\"]", "egress_ports in [vm.a]"},
		"a port as a word":       {"[vm.a]\negress_ports = [\"none\"]", `egress_ports in [vm.a]: "none" is not a range of ports`},
		"run args as a string":   {"[container.a]\nrun_args = \"--init\"", "run_args in [container.a] must be an array of strings"},
		"a run arg not text":     {"[container.a]\nrun_args = [\"--init\", 1]", "run_args in [container.a] must be an array of strings"},
		"an empty run arg":       {"[container.a]\nrun_args = [\"\"]", "must be an array of strings"},
		"open_urls":              {"[link]\nopen_urls = \"sometimes\"", `open_urls in [link]: "sometimes" is not "ask", "allow" or "off"`},
		"roots not a table":      {`roots = "/x"`, "roots must be a table"},
		"empty roots":            {"[roots]", "[roots] names no roots"},
		"a bad root name":        {"[roots]\n\"My Dev\" = \"/x\"", "'My Dev' is not a root name"},
		"a root not a path":      {"[roots]\ndev = 1", "root dev must be a path"},
		"a long root, no path":   {"[roots.dev]\nhost = \"/x\"", "root dev must be a path, or a table of exactly host"},
		"a long root, more":      {"[roots.dev]\nhost = \"/x\"\npath = \"/y\"\nmode = \"ro\"", "root dev must be a path, or a table of exactly host"},
		"bad TOML":               {"[session\n", cfgPath + ": toml"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := load(t, tc.body)
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("err %v, want %q", err, tc.err)
			}
		})
	}
}

// A file in the first, flat layout fails, naming the key, saying the
// layout changed, where the key went, and where it is documented.
func TestOldLayoutRefused(t *testing.T) {
	for body, want := range map[string]string{
		`repo_root = "~/src"`:            "repo_root is not a setting any more: config.toml's layout changed, and it is now a [roots] table",
		`no_tmux = true`:                 "it is now tmux in [session], true or false",
		`docker_run_args = ["--init"]`:   "now run_args in a [container.NAME] or [gvisor.NAME] profile",
		`vm_cpus = 4`:                    "now cpus in a [vm.NAME] profile",
		`egress_proxy = "off"`:           "now egress in a [vm.NAME] profile",
		`image = "x"`:                    "the image is named after the environment",
		"format = 1\nbase_image = \"x\"": "now base in [image]",
	} {
		_, err := load(t, body)
		if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "(see docs/configuration.md)") {
			t.Errorf("%s: %v", body, err)
		}
	}
	for _, m := range Moved {
		if m.Key == "" {
			continue
		}
		if _, err := load(t, m.Key+" = 1"); err == nil || !strings.Contains(err.Error(), m.Key+" is not a setting any more") {
			t.Errorf("%s: %v", m.Key, err)
		}
	}
}

// [roots] mounts each root at /work/<name>, in name order, or at the path
// its long form names; a ~ in one is the home dir.
func TestConfigFileRoots(t *testing.T) {
	c, err := load(t, "[roots]\nwork = \"/w\"\ndev = \"~/dev\"\n\n[roots.tools]\nhost = \"~/src/tools\"\npath = \"/opt/tools\"\n")
	if err != nil {
		t.Fatal(err)
	}
	want := []Root{{Name: "dev", Host: "/h/dev", Container: "/work/dev"}, {Name: "tools", Host: "/h/src/tools", Container: "/opt/tools"},
		{Name: "work", Host: "/w", Container: "/work/work"}}
	if !reflect.DeepEqual(c.Roots, want) || c.RootsOrigin() != "set by [roots] in "+cfgPath {
		t.Errorf("roots %+v, origin %q", c.Roots, c.RootsOrigin())
	}
	// Inline, as setup writes the long form.
	c, err = load(t, "[roots]\ntools = { host = \"/t\", path = \"/srv/tools\" }\n")
	if err != nil || !reflect.DeepEqual(c.Roots, []Root{{Name: "tools", Host: "/t", Container: "/srv/tools"}}) {
		t.Errorf("inline: %+v %v", c.Roots, err)
	}
	// A sole root may be /work itself.
	c, err = load(t, "[roots.dev]\nhost = \"/d\"\npath = \"/work\"\n")
	if err != nil || !reflect.DeepEqual(c.Roots, []Root{{Name: "dev", Host: "/d", Container: "/work"}}) {
		t.Errorf("flat: %+v %v", c.Roots, err)
	}
}

func TestConfigFileRootsRefused(t *testing.T) {
	for name, tc := range map[string]struct{ body, err string }{
		"/work with another": {"[roots]\nx = \"/x\"\n[roots.dev]\nhost = \"/d\"\npath = \"/work\"\n",
			"root x at /work/x cannot share the sandbox with dev at /work, which only a sole root may use; give dev a path of its own"},
		"/work with one elsewhere": {"[roots]\nx = { host = \"/x\", path = \"/srv/x\" }\n[roots.dev]\nhost = \"/d\"\npath = \"/work\"\n",
			"root x at /srv/x cannot share the sandbox with dev at /work"},
		"nested inside": {"[roots]\na = { host = \"/a\", path = \"/opt/a\" }\nb = { host = \"/b\", path = \"/opt/a/b\" }\n",
			"roots a and b nest in the sandbox (/opt/a, /opt/a/b)"},
		"nested around": {"[roots]\na = { host = \"/a\", path = \"/opt/a/b\" }\nb = { host = \"/b\", path = \"/opt/a\" }\n",
			"roots b and a nest in the sandbox (/opt/a, /opt/a/b)"},
		"the same path": {"[roots]\na = { host = \"/a\", path = \"/srv/x\" }\nb = { host = \"/b\", path = \"/srv/x\" }\n",
			"nest in the sandbox"},
		"inside another's default": {"[roots]\na = \"/a\"\nb = { host = \"/b\", path = \"/work/a/b\" }\n",
			"roots a and b nest in the sandbox (/work/a, /work/a/b)"},
		"a colon in the container path": {"[roots]\na = { host = \"/a\", path = \"/srv/a:ro\" }\n", `path "/srv/a:ro" has ':'`},
		"a comma in the container path": {"[roots]\na = { host = \"/a\", path = \"/srv/a,b\" }\n", `has ','`},
		"a control character":           {"[roots]\na = { host = \"/a\", path = \"/srv/a\\u0007\" }\n", "has a control character"},
		"a colon in the host path":      {"[roots]\na = \"/a:b\"\n", `root a: host path "/a:b" has ':'`},
		"a relative path":               {"[roots]\na = { host = \"/a\", path = \"work/a\" }\n", `root a: path "work/a" is not an absolute, clean path`},
		"an unclean path":               {"[roots]\na = { host = \"/a\", path = \"/work/../etc\" }\n", "is not an absolute, clean path"},
		"a relative host":               {"[roots]\na = \"src/a\"\n", "root a: src/a is not an absolute path (or one starting with ~/)"},
		"the whole sandbox":             {"[roots]\na = { host = \"/a\", path = \"/\" }\n", `would cover the whole sandbox`},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := load(t, tc.body)
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("err %v, want %q", err, tc.err)
			}
		})
	}
}

func TestCheckContainerPath(t *testing.T) {
	for _, ok := range []string{"/work", "/work/a", "/opt/tools", "/srv/x", "/code", "/workspace/a"} {
		if err := CheckContainerPath(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"/", "/etc", "/etc/x", "/usr/local/src", "/home", "/home/agent/src", "/lib", "/lib64/x",
		"/libx32", "/proc", "/sys/x", "/dev", "/var/run", "/tmp/x", "/run", "/boot", "/root", "/bin", "/sbin",
		"/ssh-agent.sock", "/opt", "/mnt", "/media", "/srv", "relative", "/a/../b", "/a/"} {
		if err := CheckContainerPath(bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
	// The home is ContainerHome's parent, and caboose's mounts are in it.
	if CheckContainerPath(ContainerHome+"/.claude") == nil {
		t.Error("a root over caboose's own mounts accepted")
	}
}

func TestFormat(t *testing.T) {
	for data, want := range map[string]int{"": 0, "format = 1\n": 1} {
		f, err := ParseFile("c.toml", []byte(data))
		if err != nil || f.Format != want {
			t.Errorf("%q: %+v %v", data, f, err)
		}
	}
	for data, want := range map[string]string{
		"format = 2\n":             "c.toml is format 2, and this caboose reads up to format 1",
		"format = 2\nfuture = 1\n": "c.toml is format 2, and this caboose reads up to format 1",
		"format = 0\n":             "format must be a whole number",
		"format = \"1\"\n":         "format must be a whole number",
	} {
		if _, err := ParseFile("c.toml", []byte(data)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want %q", data, err, want)
		}
	}
}

// The template parses, sets nothing but format, and names every key there
// is, each under its own table.
func TestTemplate(t *testing.T) {
	f, err := ParseFile("c.toml", []byte(Template))
	if err != nil || f.Format != FileFormat || len(f.Vals) != 1 || f.Roots != nil || f.Profiles != nil {
		t.Fatalf("the template: %+v %v", f, err)
	}
	for table, keys := range tables {
		if table != "" && !strings.Contains(Template, "#["+table+"]\n") {
			t.Errorf("the template has no #[%s]", table)
		}
		for k := range keys {
			if !strings.Contains(Template, "\n#"+k+" = ") && k != "format" {
				t.Errorf("the template has no #%s", k)
			}
		}
	}
	for kind, keys := range profileKeys {
		if !strings.Contains(Template, "#["+kind+".default]\n") {
			t.Errorf("the template has no #[%s.default]", kind)
		}
		for k := range keys {
			if !strings.Contains(Template, "\n#"+k+" = ") {
				t.Errorf("the template has no #%s", k)
			}
		}
	}
	// Uncommented whole, it is a valid file of every table.
	var lines []string
	for _, l := range strings.Split(Template, "\n") {
		if len(l) > 1 && l[0] == '#' && l[1] != ' ' {
			l = l[1:]
		}
		lines = append(lines, l)
	}
	c, err := Load(envOf(map[string]string{"HOME": "/h"}), fakeFS{cfgPath: strings.Join(lines, "\n")}, "")
	if err != nil || c.Profile != "gvisor.default" || len(c.Roots) != 2 {
		t.Errorf("uncommented: %+v %v", c, err)
	}
}
