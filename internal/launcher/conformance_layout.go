package launcher

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/conformance"
	"github.com/bfreis/caboose/internal/conformance/checks"
	"github.com/bfreis/caboose/internal/daemon"
	"github.com/bfreis/caboose/internal/datadir"
	"github.com/bfreis/caboose/internal/nofollow"
)

// The conformance suite's throwaway environment, as files: its
// config.toml, written by the suite and naming nothing outside B, and the
// canaries its checks need.

// confProfile is the name of the suite's isolation profile.
const confProfile = "conformance"

// confReadyTimeout bounds the first start, which installs Claude Code
// into the throwaway data dir.
const confReadyTimeout = 900

// confSettings are what the suite's config.toml says.
type confSettings struct {
	Isolation, Trust string
	// Roots are the two roots, by name, read-only then the one with the
	// writable part, at their host paths under B.
	Roots [2]config.Root
	// OpenURLs is [link] open_urls: "allow", since the suite's stub
	// opener is first on the PATH of the daemon its link runs in
	// (conformance.StubDir), so a URL the link opens reaches a stub, never
	// a browser.
	OpenURLs string
	// EgressAllow is a vm profile's egress_allow.
	EgressAllow []string
	// Checkpoints are [checkpoints] paths: container paths.
	Checkpoints []string
	// Aliases are the suite's [alias.NAME] tables.
	Aliases []conformance.SuiteAlias
}

// confSettingsFor are the settings of the run g makes.
func confSettingsFor(g *conformance.Guard) confSettings {
	ro, dev := g.RootNames()[0], g.RootNames()[1]
	s := confSettings{
		Isolation: g.Isolation(),
		Trust:     g.Trust(),
		Roots: [2]config.Root{
			{Name: ro, Host: g.RootPath(ro), Container: config.WorkDir + "/" + ro},
			{Name: dev, Host: g.RootPath(dev), Container: config.WorkDir + "/" + dev, Writable: []string{conformance.WritablePart}},
		},
		OpenURLs:    "allow",
		Checkpoints: []string{config.WorkDir + "/" + dev + "/" + conformance.WritablePart},
	}
	// The suite's aliases, and no other: scripts in the stubs' directory
	// (outside every root), writing only into the alias-out canary.
	aliases, err := conformance.SuiteAliases(g.StubPath(), confAliasOut(g), g.RootPath(dev)+"/"+conformance.WritablePart)
	if err == nil {
		s.Aliases = aliases
	}
	if g.Isolation() == config.KindVM && g.Trust() == config.TrustHostile {
		// A hostile launch refuses until egress_allow names the hosts
		// Claude Code reaches: the first start installs it.
		s.EgressAllow = slices.Clone(claudeHosts)
	}
	return s
}

// confAliasOut is the host path of the alias-out canary, where the
// suite's aliases write: canary/conf-<n8>-alias-out.
func confAliasOut(g *conformance.Guard) string {
	return g.Base() + "/" + confCanaryDir + "/" + g.CanaryName(checks.RoleAliasOut)
}

// confCanaryDir is the directory of canaries no mount reaches, relative to B.
const confCanaryDir = "canary"

// tomlString quotes s as a TOML basic string; it refuses anything that
// would need an escape, which no value the suite writes has.
func tomlString(s string) (string, error) {
	for _, r := range s {
		if r < 0x20 || r == 0x7f || r == '"' || r == '\\' {
			return "", fmt.Errorf("conformance: %q cannot go in the suite's config.toml", s)
		}
	}
	return `"` + s + `"`, nil
}

func tomlStrings(ss []string) (string, error) {
	q := make([]string, len(ss))
	for i, s := range ss {
		v, err := tomlString(s)
		if err != nil {
			return "", err
		}
		q[i] = v
	}
	return "[" + strings.Join(q, ", ") + "]", nil
}

var confKeyRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// confConfigText is the suite's config.toml.
func confConfigText(s confSettings) (string, error) {
	if !slices.Contains(config.Kinds, s.Isolation) {
		return "", fmt.Errorf("conformance: unknown isolation %q", s.Isolation)
	}
	if s.OpenURLs != "off" && s.OpenURLs != "allow" {
		return "", fmt.Errorf("conformance: open_urls %q", s.OpenURLs)
	}
	var b strings.Builder
	var err error
	q := func(v string) string {
		out, e := tomlString(v)
		if e != nil && err == nil {
			err = e
		}
		return out
	}
	qs := func(v []string) string {
		out, e := tomlStrings(v)
		if e != nil && err == nil {
			err = e
		}
		return out
	}
	fmt.Fprintf(&b, "format = %d\n", config.FileFormat)
	fmt.Fprintf(&b, "isolation = %s\n", q(s.Isolation+"."+confProfile))
	fmt.Fprintf(&b, "trust = %s\n", q(s.Trust))
	for _, r := range s.Roots {
		if !confKeyRe.MatchString(r.Name) {
			return "", fmt.Errorf("conformance: root name %q", r.Name)
		}
		fmt.Fprintf(&b, "\n[roots.%s]\nhost = %s\npath = %s\n", r.Name, q(r.Host), q(r.Container))
		if len(r.Writable) > 0 {
			fmt.Fprintf(&b, "writable = %s\n", qs(r.Writable))
		}
	}
	fmt.Fprintf(&b, "\n[session]\nready_timeout = %d\n", confReadyTimeout)
	fmt.Fprintf(&b, "\n[link]\nhost_exec = false\nforward_ports = []\nopen_urls = %s\nssh_agent = \"none\"\n", q(s.OpenURLs))
	if len(s.Checkpoints) > 0 {
		fmt.Fprintf(&b, "\n[checkpoints]\npaths = %s\n", qs(s.Checkpoints))
	}
	fmt.Fprintf(&b, "\n[%s.%s]\n", s.Isolation, confProfile)
	if s.Isolation == config.KindVM {
		fmt.Fprintf(&b, "egress_allow = %s\n", qs(s.EgressAllow))
	}
	for _, al := range s.Aliases {
		if !confKeyRe.MatchString(al.Name) {
			return "", fmt.Errorf("conformance: alias name %q", al.Name)
		}
		fmt.Fprintf(&b, "\n[alias.%s]\ncommand = %s\npolicy = %s\n", al.Name, qs(al.Command), q(al.Policy))
		if al.Cwd != "" {
			fmt.Fprintf(&b, "cwd = %s\n", q(al.Cwd))
		}
		if len(al.Params) > 0 {
			fmt.Fprintf(&b, "\n[alias.%s.params]\n", al.Name)
			for _, k := range slices.Sorted(maps.Keys(al.Params)) {
				if !confKeyRe.MatchString(k) {
					return "", fmt.Errorf("conformance: alias parameter %q", k)
				}
				fmt.Fprintf(&b, "%s = %s\n", k, qs(al.Params[k]))
			}
		}
	}
	if err != nil {
		return "", err
	}
	return b.String(), nil
}

// confConfigRel is the environment's config.toml, relative to B.
func confConfigRel(env string) string { return "home/envs/" + env + "/" + config.FileName }

// writeConfConfig writes the suite's config.toml into B, through
// nofollow, making the environment's directory. It never replaces a file.
func writeConfConfig(g *conformance.Guard, s confSettings) error {
	text, err := confConfigText(s)
	if err != nil {
		return err
	}
	rel := confConfigRel(g.Env())
	if err := g.MakeDir(path.Dir(rel)); err != nil {
		return err
	}
	d := nofollow.Dir(g.Base())
	if _, err := d.Lstat(rel); err == nil {
		return fmt.Errorf("conformance: %s exists already", rel)
	}
	return d.WriteFile(rel, []byte(text), 0o600)
}

// checkConfConfig loads the written config.toml with the launcher's own
// loader, as the launch will, and refuses one that does not say what the
// suite wrote: the isolation, the trust, the two roots, no host exec, no
// SSH agent, no forwarded port, no run_args or engine socket.
func checkConfConfig(cfg *config.Config, s confSettings) error {
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }
	if cfg.Isolation != s.Isolation {
		add("isolation %q", cfg.Isolation)
	}
	if cfg.Trust != s.Trust {
		add("trust %q", cfg.Trust)
	}
	if len(cfg.Roots) != len(s.Roots) {
		add("%d roots", len(cfg.Roots))
	}
	for _, r := range cfg.Roots {
		i := slices.IndexFunc(s.Roots[:], func(w config.Root) bool { return w.Name == r.Name })
		if i < 0 {
			add("root %s", r.Name)
			continue
		}
		w := s.Roots[i]
		if filepath.Clean(r.Host) != w.Host || r.Container != w.Container || !slices.Equal(r.Writable, w.Writable) {
			add("root %s", r.Name)
		}
	}
	if len(cfg.Aliases) != len(s.Aliases) {
		add("%d aliases", len(cfg.Aliases))
	}
	for _, al := range cfg.Aliases {
		i := slices.IndexFunc(s.Aliases, func(w conformance.SuiteAlias) bool { return w.Name == al.Name })
		if i < 0 || !slices.Equal(al.Command, s.Aliases[i].Command) || al.Policy != s.Aliases[i].Policy || al.Cwd != s.Aliases[i].Cwd {
			add("alias %s", al.Name)
		}
	}
	if cfg.OpenURLs != s.OpenURLs {
		add("open_urls %q", cfg.OpenURLs)
	}
	switch {
	case cfg.HostExec:
		add("host_exec on")
	case cfg.SSHAgent != "none":
		add("ssh_agent %q", cfg.SSHAgent)
	case cfg.ForwardPorts != "none":
		add("forward_ports %q", cfg.ForwardPorts)
	case len(cfg.RunArgs) > 0:
		add("run_args")
	case cfg.EngineSocket:
		add("engine_socket on")
	}
	if len(problems) > 0 {
		return errors.New("conformance: the suite's config.toml does not load as written: " + strings.Join(problems, ", "))
	}
	return nil
}

// confPaths are the directories, relative to B, the file checks' canaries
// go in.
func confPaths(g *conformance.Guard) checks.Paths {
	ro, dev := g.RootNames()[0], g.RootNames()[1]
	return checks.Paths{
		RO:      "roots/" + ro,
		Dev:     "roots/" + dev,
		Part:    "roots/" + dev + "/" + conformance.WritablePart,
		Managed: g.DataRel() + "/" + datadir.ManagedDir,
		Canary:  confCanaryDir,
	}
}

// confCkptDirs are the directories, relative to B, the checkpoint checks'
// canaries go in.
func confCkptDirs(g *conformance.Guard) checks.CheckpointDirs {
	p := confPaths(g)
	return checks.CheckpointDirs{
		Part: p.Part, Canary: p.Canary, Store: g.StoreRel(), EnvDir: g.EnvDirRel(), Home: "home",
		DataKeep: g.DataRel() + "/home/.config/caboose",
	}
}

// confLayout splits the canaries every group's checks need into those
// made before the launch, those of the checkpoint store, made once the
// store is (store), and those made after the launch: the managed
// directory's, which the launch writes.
func confLayout(g *conformance.Guard) (before, store, after []checks.Spec, err error) {
	p := confPaths(g)
	specs, err := checks.Layout(p)
	if err != nil {
		return nil, nil, nil, err
	}
	host, err := checks.HostLayout(p.Canary, p.RO)
	if err != nil {
		return nil, nil, nil, err
	}
	ckpt, err := checks.CheckpointLayout(confCkptDirs(g))
	if err != nil {
		return nil, nil, nil, err
	}
	// One canary per role. The file checks' watched "ckpt" (a file no
	// mount reaches, which they look for in the canary directory) and the
	// checkpoint checks' store canary share the role: it is made once, a
	// file in the checkpoint store, which no mount reaches either (the
	// audit refuses any source that overlaps it), so both groups' checks
	// hold of it.
	var all []checks.Spec
	for _, groups := range [][]checks.HostCanary{ckpt, host} {
		for _, h := range groups {
			all = append(all, checks.Spec(h))
		}
	}
	all = append(all, specs...)
	seen := map[string]checks.Spec{}
	for _, s := range all {
		if old, ok := seen[s.Role]; ok {
			if old != s && !(old.Parent == g.StoreRel() && s.Parent == p.Canary && old.Kind == s.Kind) {
				return nil, nil, nil, fmt.Errorf("conformance: canary %q is wanted in two places", s.Role)
			}
			continue
		}
		seen[s.Role] = s
		switch {
		case s.Parent == p.Managed:
			after = append(after, s)
		case s.Parent == g.StoreRel():
			store = append(store, s)
		default:
			before = append(before, s)
		}
	}
	return before, store, after, nil
}

// confLayoutDirs are the directories, relative to B, the canaries' parents
// that the launch does not make before them: the environment's directory,
// the data dir's ~/.config/caboose, and the daemon checks' directories no
// mount reaches.
func confLayoutDirs(g *conformance.Guard) []string {
	d := confDaemonDirs(g, "")
	return []string{g.EnvDirRel(), confCkptDirs(g).DataKeep, d.Outside, d.Creds}
}

// confDaemonDirs are the directories, relative to B, the daemon checks'
// canaries go in; daemonRel is the suite's daemon's directory. Their home
// canaries go in the data dir's home/.config/caboose, which the sandbox
// has at ~/.config/caboose (the sandbox config always keeps it), as the
// checkpoint checks' do.
func confDaemonDirs(g *conformance.Guard, daemonRel string) checks.DaemonDirs {
	return checks.DaemonDirs{
		Part: confPaths(g).Part, Home: confCkptDirs(g).DataKeep,
		Outside: confCanaryDir + "/outside-root", Creds: confCanaryDir + "/creds",
		Daemon: daemonRel, Env: g.EnvDirRel(), Checkpoints: g.StoreRel(),
	}
}

// confDaemonCanaries are the daemon checks' canaries, for the suite's
// daemon in daemonDir (inside the throwaway CABOOSE_HOME), made once the
// daemon has made it.
func confDaemonCanaries(g *conformance.Guard, daemonDir string) ([]checks.Spec, error) {
	if !confInside(daemonDir, g.CabooseHome()) || !slices.Contains(daemon.Dirs(g.CabooseHome()), filepath.Clean(daemonDir)) {
		return nil, fmt.Errorf("conformance: %s is not one of %s's daemon directories", daemonDir, g.CabooseHome())
	}
	rel, err := filepath.Rel(g.Base(), daemonDir)
	if err != nil {
		return nil, err
	}
	layout, err := checks.DaemonLayout(confDaemonDirs(g, filepath.ToSlash(rel)))
	if err != nil {
		return nil, err
	}
	out := make([]checks.Spec, len(layout))
	for i, h := range layout {
		out[i] = checks.Spec(h)
	}
	return out, nil
}

// writeConfScripts writes the suite's alias scripts at their commands,
// from the host and never through a symlink: each into the stubs'
// directory, which conformance.WriteStubs made.
func writeConfScripts(g *conformance.Guard, aliases []conformance.SuiteAlias) error {
	d := nofollow.Dir(g.Base())
	done := map[string]bool{}
	for _, al := range aliases {
		if len(al.Command) == 0 {
			return fmt.Errorf("conformance: alias %s has no command", al.Name)
		}
		cmd := al.Command[0]
		if done[cmd] {
			continue
		}
		if path.Dir(cmd) != g.StubPath() {
			return fmt.Errorf("conformance: alias %s's command %s is not in the stubs' directory", al.Name, cmd)
		}
		rel := conformance.StubDir + "/" + path.Base(cmd)
		if _, err := d.Lstat(rel); err == nil {
			return fmt.Errorf("conformance: %s exists already", rel)
		}
		if err := d.WriteFile(rel, []byte(al.Script), 0o755); err != nil {
			return err
		}
		done[cmd] = true
	}
	return nil
}

// placeConfProbe copies the probe for goarch from probeDir (what make
// conformance-probe built: conformance-probe-linux-<arch>) into the
// writable part, roots/conf-<n8>-dev/part/.conformance-probe, from the
// host and never through a symlink. No probeDir places nothing.
func placeConfProbe(g *conformance.Guard, probeDir, goarch string) error {
	if probeDir == "" {
		return nil
	}
	if goarch != "amd64" && goarch != "arm64" {
		return fmt.Errorf("conformance: no probe for %s", goarch)
	}
	if !filepath.IsAbs(probeDir) || filepath.Clean(probeDir) != probeDir {
		return fmt.Errorf("conformance: --probe-dir %q is not an absolute clean path", probeDir)
	}
	b, _, err := nofollow.Dir(probeDir).ReadFileMax("conformance-probe-linux-"+goarch, confProbeMax)
	if err != nil {
		return fmt.Errorf("conformance: the probe: %w", err)
	}
	rel, err := conformance.ProbeRel(g.Nonce8())
	if err != nil {
		return err
	}
	if rel != confPaths(g).Part+"/"+conformance.ProbeName {
		return fmt.Errorf("conformance: the probe's place %s is not in the writable part", rel)
	}
	d := nofollow.Dir(g.Base())
	if _, err := d.Lstat(rel); err == nil {
		return fmt.Errorf("conformance: %s exists already", rel)
	}
	if err := d.WriteFile(rel, b, 0o755); err != nil {
		return err
	}
	sum := sha256.Sum256(b)
	return g.RecordProbe(hex.EncodeToString(sum[:]))
}

// confProbeMax bounds the probe's size.
const confProbeMax = 64 << 20

// makeCanaries makes specs through h.
func makeCanaries(h conformance.Host, specs []checks.Spec) error {
	for _, s := range specs {
		if _, err := h.MakeCanary(s.Parent, s.Role, s.Kind); err != nil {
			return fmt.Errorf("conformance: canary %s: %w", s.Role, err)
		}
	}
	return nil
}
