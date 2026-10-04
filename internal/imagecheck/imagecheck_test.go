package imagecheck

import (
	"slices"
	"strings"
	"testing"
)

// Canned probe output, shaped as imagecheck.sh prints it for the images the
// design names. probe_test.go runs the real script; these pin the verdict.

var tools = []string{
	"ok tool:env /usr/bin/env", "ok tool:readlink /usr/bin/readlink", "ok tool:mktemp /usr/bin/mktemp",
	"ok tool:find /usr/bin/find", "ok tool:rm /usr/bin/rm", "ok tool:rmdir /usr/bin/rmdir",
	"ok tool:sleep /usr/bin/sleep", "ok tool:test /usr/bin/test", "ok tool:uname /usr/bin/uname",
	"ok tool:mkdir /usr/bin/mkdir", "ok tool:chmod /usr/bin/chmod", "ok tool:cut /usr/bin/cut",
	"ok tool:sed /usr/bin/sed", "ok tool:tr /usr/bin/tr", "ok tool:grep /usr/bin/grep",
	"ok tool:head /usr/bin/head", "ok tool:sha256sum /usr/bin/sha256sum", "ok tool:chown /usr/bin/chown",
	"ok etc:passwd /etc/passwd", "ok etc:group /etc/group", "ok home /home/agent (created by the layer)",
}

func probeOut(lines ...[]string) string {
	var all []string
	for _, l := range lines {
		all = append(all, l...)
	}
	return "probe 1\n" + strings.Join(all, "\n") + "\nend\n"
}

var (
	debianFull = probeOut(
		[]string{"ok sh /bin/sh", "ok bash /usr/bin/bash", "ok curl /usr/bin/curl", "ok tmux /usr/bin/tmux", "ok git /usr/bin/git 2.53.0", "ok tic /usr/bin/tic"},
		tools,
		[]string{"info libc glibc", "ok libc glibc /lib/x86_64-linux-gnu/libc.so.6",
			"info arch x86_64", "ok arch x86_64", "info platform linux-x64",
			"info uid 1000", "info gid 1000",
			"ok net https://claude.ai", "ok net https://downloads.claude.ai",
			"ok cacerts /etc/ssl/certs/ca-certificates.crt"})

	// debian:stable as it comes: bash and tic (ncurses-bin is essential),
	// but no curl, no CA bundle, no tmux, no git, and no curl to test the
	// network.
	debianBare = probeOut(
		[]string{"ok sh /bin/sh", "ok bash /usr/bin/bash", "missing curl", "missing tmux", "missing git", "ok tic /usr/bin/tic"},
		tools,
		[]string{"info libc glibc", "ok libc glibc /lib/aarch64-linux-gnu/libc.so.6",
			"info arch aarch64", "ok arch aarch64", "info platform linux-arm64",
			"info uid 501", "info gid 20 dialout",
			"warn net https://claude.ai not checked: no curl", "warn net https://downloads.claude.ai not checked: no curl",
			"missing cacerts no bundle at the usual paths"})

	// alpine as it comes: BusyBox covers the tools, and the CA bundle is in
	// the base image, but there is no bash, curl, tmux or git, none of the
	// musl build's libraries, and no ripgrep.
	alpineBare = probeOut(
		[]string{"ok sh /bin/sh", "missing bash", "missing curl", "missing tmux", "missing git", "missing tic"},
		busybox(tools),
		[]string{"info libc musl", "ok libc musl /lib/libc.musl-aarch64.so.1",
			"info arch aarch64", "ok arch aarch64", "info platform linux-arm64-musl",
			"missing libgcc no libgcc_s.so.1", "missing libstdc++ no libstdc++.so.6", "missing ripgrep",
			"info uid 1000", "info gid 1000",
			"warn net https://claude.ai not checked: no curl", "warn net https://downloads.claude.ai not checked: no curl",
			"ok cacerts /etc/ssl/certs/ca-certificates.crt"})

	alpineFull = probeOut(
		[]string{"ok sh /bin/sh", "ok bash /bin/bash", "ok curl /usr/bin/curl", "ok tmux /usr/bin/tmux", "ok git /usr/bin/git 2.52.0", "missing tic"},
		busybox(tools),
		[]string{"info libc musl", "ok libc musl /lib/libc.musl-x86_64.so.1",
			"info arch x86_64", "ok arch x86_64", "info platform linux-x64-musl",
			"ok libgcc /usr/lib/libgcc_s.so.1", "ok libstdc++ /usr/lib/libstdc++.so.6", "ok ripgrep /usr/bin/rg",
			"info uid 1000", "info gid 1000",
			"ok net https://claude.ai", "ok net https://downloads.claude.ai",
			"ok cacerts /etc/ssl/certs/ca-certificates.crt"})

	// node:22 style: complete, but the host UID is node's, and an agent
	// user exists already.
	nodeImage = strings.Replace(strings.Replace(debianFull,
		"info uid 1000\n", "info uid 1000 node\n", 1),
		"info gid 1000\n", "info gid 1000 node\ninfo user agent 1001\ninfo group agent 1001\n", 1)

	// Neither glibc nor musl, on an architecture Claude Code has no build for.
	oddImage = strings.Replace(strings.Replace(strings.Replace(debianFull,
		"info libc glibc\nok libc glibc /lib/x86_64-linux-gnu/libc.so.6\n", "info libc unknown\nmissing libc neither glibc nor musl\n", 1),
		"info arch x86_64\nok arch x86_64\ninfo platform linux-x64\n", "info arch riscv64\nmissing arch riscv64\n", 1),
		"ok net https://claude.ai\n", "warn net https://claude.ai unreachable (curl exit 6)\n", 1)
)

// busybox moves the tools to /bin, where BusyBox links most of them.
func busybox(lines []string) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = strings.Replace(l, " /usr/bin/", " /bin/", 1)
	}
	return out
}

func mustParse(t *testing.T, out string) *Report {
	t.Helper()
	r, err := Parse(out)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestParseVerdicts(t *testing.T) {
	cases := []struct {
		name     string
		out      string
		libc     string
		platform string
		problems []string // each problem must start with one of these, in order
	}{
		{"debian, complete", debianFull, "glibc", "linux-x64", nil},
		{"debian:stable", debianBare, "glibc", "linux-arm64",
			[]string{"curl: missing", "CA certificates: missing (no bundle at the usual paths)", "tmux: missing", "git: missing"}},
		{"alpine", alpineBare, "musl", "linux-arm64-musl",
			[]string{"bash: missing", "curl: missing", "tmux: missing", "git: missing",
				"libgcc: missing (no libgcc_s.so.1)", "libstdc++: missing (no libstdc++.so.6)", "ripgrep: missing"}},
		{"alpine, complete", alpineFull, "musl", "linux-x64-musl", nil},
		{"uid held by node", nodeImage, "glibc", "linux-x64", nil},
		{"unsupported libc and arch", oddImage, "", "",
			[]string{"glibc or musl: missing (neither glibc nor musl)", "x86_64 or aarch64: missing (riscv64)"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := mustParse(t, tc.out)
			if r.Libc != tc.libc || r.Platform != tc.platform {
				t.Errorf("libc %q platform %q, want %q %q", r.Libc, r.Platform, tc.libc, tc.platform)
			}
			ps := r.Problems()
			if len(ps) != len(tc.problems) {
				t.Fatalf("problems:\n%s", strings.Join(ps, "\n"))
			}
			for i, p := range ps {
				if !strings.HasPrefix(p, tc.problems[i]+" -- ") {
					t.Errorf("problem %d = %q, want it to start %q", i, p, tc.problems[i])
				}
			}
			if r.OK() != (len(tc.problems) == 0) {
				t.Errorf("OK() = %v", r.OK())
			}
		})
	}
}

func TestParseFacts(t *testing.T) {
	r := mustParse(t, nodeImage)
	if r.UID != 1000 || r.UIDOwner != "node" || r.GID != 1000 || r.GIDOwner != "node" {
		t.Errorf("uid %d %q gid %d %q", r.UID, r.UIDOwner, r.GID, r.GIDOwner)
	}
	if r.AgentUID != "1001" || r.AgentGID != "1001" {
		t.Errorf("agent %q %q", r.AgentUID, r.AgentGID)
	}
	if r.Arch != "x86_64" {
		t.Errorf("arch %q", r.Arch)
	}
	if c, ok := r.Check("tic"); !ok || !c.OK || c.Detail != "/usr/bin/tic" {
		t.Errorf("tic = %+v, %v", c, ok)
	}

	r = mustParse(t, debianBare)
	if r.UID != 501 || r.UIDOwner != "" || r.GIDOwner != "dialout" {
		t.Errorf("uid %d %q gid owner %q", r.UID, r.UIDOwner, r.GIDOwner)
	}
	if len(r.Warnings) != 2 || r.Warnings[0] != "net https://claude.ai not checked: no curl" {
		t.Errorf("warnings %q", r.Warnings)
	}

	r = mustParse(t, oddImage)
	if r.Arch != "riscv64" || r.Libc != "" {
		t.Errorf("arch %q libc %q", r.Arch, r.Libc)
	}
}

// Optional and not-applicable checks never count against an image: tic is
// optional, and the musl libraries are only asked of musl.
func TestOptionalAndMuslOnly(t *testing.T) {
	out := strings.Replace(debianFull, "ok tic /usr/bin/tic", "missing tic", 1)
	if r := mustParse(t, out); !r.OK() {
		t.Errorf("a missing tic failed the image: %q", r.Problems())
	}
	// debianFull reports no libgcc at all, which only matters on musl.
	if _, ok := mustParse(t, debianFull).Check("libgcc"); ok {
		t.Fatal("fixture has a libgcc line")
	}
}

// A check the probe did not report at all is unmet, not passed: a probe
// that skipped something must not wave the image through.
func TestUnreportedCheckIsUnmet(t *testing.T) {
	out := strings.Replace(debianFull, "ok tmux /usr/bin/tmux\n", "", 1)
	ps := mustParse(t, out).Problems()
	if len(ps) != 1 || !strings.HasPrefix(ps[0], "tmux: missing (not reported by the probe)") {
		t.Errorf("problems %q", ps)
	}
	out = strings.Replace(alpineFull, "ok ripgrep /usr/bin/rg\n", "", 1)
	if ps := mustParse(t, out).Problems(); len(ps) != 1 || !strings.HasPrefix(ps[0], "ripgrep: ") {
		t.Errorf("problems %q", ps)
	}
}

func TestParseRejects(t *testing.T) {
	for name, out := range map[string]string{
		"empty":           "",
		"no header":       "ok sh /bin/sh\nend\n",
		"other version":   "probe 2\nend\n",
		"truncated":       strings.TrimSuffix(debianFull, "end\n"),
		"foreign line":    strings.Replace(debianFull, "ok tmux", "Welcome to the image!\nok tmux", 1),
		"unknown status":  strings.Replace(debianFull, "ok tmux", "maybe tmux", 1),
		"no name":         strings.Replace(debianFull, "ok tmux /usr/bin/tmux", "ok", 1),
		"bad uid":         strings.Replace(debianFull, "info uid 1000", "info uid root", 1),
		"banner on top":   "hello\n" + debianFull,
		"trailing output": debianFull + "more\n",
	} {
		t.Run(name, func(t *testing.T) {
			if r, err := Parse(out); err == nil {
				t.Errorf("parsed: %+v", r)
			}
		})
	}
}

// Each row says how it stands: a requirement met or not, and what is only
// worth knowing -- an optional tool, the network.
func TestChecklistLevels(t *testing.T) {
	levels := map[string]Level{}
	for _, r := range mustParse(t, alpineBare).Checklist() {
		levels[r.Label] = r.Level
	}
	for label, want := range map[string]Level{
		"/bin/sh": Met, "ca-certs": Met, "libc": Met, "platform": Met, "tools": Met, "user": Met, "uid": Met, "gid": Met,
		"bash": Unmet, "curl": Unmet, "libgcc": Unmet, "ripgrep": Unmet,
		"tic": Noted, "network": Noted,
	} {
		if got, ok := levels[label]; !ok || got != want {
			t.Errorf("%s: level %v (present %v), want %v", label, got, ok, want)
		}
	}
}

func TestChecklist(t *testing.T) {
	rows := func(out string) string {
		var b strings.Builder
		for _, r := range mustParse(t, out).Checklist() {
			b.WriteString(r.Label + ": " + r.Value + "\n")
		}
		return b.String()
	}
	if got, want := rows(alpineBare), `/bin/sh: ok
bash: missing
curl: missing
ca-certs: ok (/etc/ssl/certs/ca-certificates.crt)
tmux: missing
git: missing
libc: musl
platform: linux-arm64-musl
libgcc: missing (no libgcc_s.so.1)
libstdc++: missing (no libstdc++.so.6)
ripgrep: missing
tools: ok
user: ok
tic: missing (optional)
uid: 1000 (free)
gid: 1000 (free)
network: warning: https://claude.ai not checked: no curl; https://downloads.claude.ai not checked: no curl
`; got != want {
		t.Errorf("alpine checklist:\n%s\nwant:\n%s", got, want)
	}

	got := rows(nodeImage)
	for _, w := range []string{
		"bash: ok (/usr/bin/bash)\n",
		"libc: glibc\nplatform: linux-x64\ntools: ok\nuser: ok\ntic: ok (/usr/bin/tic)\n",
		"uid: 1000 (held by user 'node'; the layer takes it over as agent)\n",
		"gid: 1000 (held by group 'node'; the layer makes it agent's group, as it is)\n",
		"agent: the image already has user agent (uid 1001) and group agent (gid 1001)\n",
		"network: ok (https://claude.ai reachable)\n",
	} {
		if !strings.Contains(got, w) {
			t.Errorf("node checklist lacks %q:\n%s", w, got)
		}
	}
	if strings.Contains(got, "libgcc") {
		t.Errorf("a glibc image got musl rows:\n%s", got)
	}

	got = rows(strings.Replace(strings.Replace(debianFull,
		"ok tool:find /usr/bin/find", "missing tool:find no -mindepth/-delete", 1),
		"ok tool:sha256sum /usr/bin/sha256sum", "missing tool:sha256sum", 1))
	if !strings.Contains(got, "tools: missing find -delete (no -mindepth/-delete), sha256sum\n") {
		t.Errorf("tools row:\n%s", got)
	}
	if got := rows(strings.Replace(debianFull, "info uid 1000\n", "info uid 1000 agent\n", 1)); !strings.Contains(got, "uid: 1000 (already user 'agent')\n") {
		t.Errorf("agent's own uid:\n%s", got)
	}
	if r := mustParse(t, debianBare); r.Unreachable() {
		t.Error("not checked counted as unreachable")
	}
	if r := mustParse(t, oddImage); !r.Unreachable() {
		t.Error("curl exit 6 is not unreachable")
	}
	got = rows(oddImage)
	if !strings.Contains(got, "libc: unsupported (neither glibc nor musl)\nplatform: unsupported (arch riscv64)\n") {
		t.Errorf("odd checklist:\n%s", got)
	}
}

// A host user that is root, or in group root, is refused whatever the
// image: the layer would take over the image's root user, or make root
// agent's group. A report that never said which IDs it was asked about is
// not taken for root.
func TestRootIsAProblem(t *testing.T) {
	r := mustParse(t, strings.Replace(strings.Replace(debianFull,
		"info uid 1000\n", "info uid 0 root\n", 1), "info gid 1000\n", "info gid 0 root\n", 1))
	ps := r.Problems()
	if len(ps) != 2 || !strings.HasPrefix(ps[0], "uid: 0 -- caboose must not run as root: the layer would take over the image's root user") ||
		!strings.HasPrefix(ps[1], "gid: 0 -- ") {
		t.Errorf("problems %q", ps)
	}
	rows := r.Checklist()
	if !slices.Contains(rows, Row{"uid", "0 (root: caboose must not run as root)", Unmet}) ||
		!slices.Contains(rows, Row{"gid", "0 (root: caboose must not run with group root)", Unmet}) {
		t.Errorf("checklist %v", rows)
	}
	// Only the gid: a user whose primary group is root.
	r = mustParse(t, strings.Replace(debianFull, "info gid 1000\n", "info gid 0 root\n", 1))
	if ps := r.Problems(); len(ps) != 1 || !strings.HasPrefix(ps[0], "gid: 0") {
		t.Errorf("gid only: %q", ps)
	}
	// Even with no shell to probe with.
	nosh := &Report{NoShell: true, UID: 0, GID: 20, uidKnown: true, gidKnown: true}
	if ps := nosh.Problems(); len(ps) != 2 || !strings.HasPrefix(ps[0], "uid: 0") {
		t.Errorf("no shell: %q", ps)
	}
	unasked := mustParse(t, strings.Replace(strings.Replace(debianFull, "info uid 1000\n", "", 1), "info gid 1000\n", "", 1))
	if ps := unasked.Problems(); len(ps) != 0 {
		t.Errorf("no IDs reported: %q", ps)
	}
}

// A host UID held by a system account (Debian's _apt is 100) is taken over
// like any other -- refusing would leave that host user no way to run
// caboose at all -- but the checklist and Notes say what it is.
func TestSystemAccountTakeover(t *testing.T) {
	r := mustParse(t, strings.Replace(debianFull, "info uid 1000\n",
		"info uid 100 _apt\ninfo uid-home /nonexistent\ninfo uid-shell /usr/sbin/nologin\n", 1))
	if !r.SystemAccount() || !r.OK() {
		t.Errorf("system %v, problems %q", r.SystemAccount(), r.Problems())
	}
	if !slices.Contains(r.Checklist(), Row{"uid", "100 (held by system account '_apt', home /nonexistent, shell /usr/sbin/nologin; the layer takes it over as agent)", Noted}) {
		t.Errorf("checklist %v", r.Checklist())
	}
	if n := r.Notes(); len(n) != 1 || !strings.Contains(n[0], "system account '_apt'") ||
		!strings.Contains(n[0], "whatever the image runs as '_apt' will run as agent") {
		t.Errorf("notes %q", n)
	}
	// A login user -- ubuntu, node -- is not one, nor is an empty shell
	// field, which means /bin/sh.
	for _, shell := range []string{"/bin/bash", ""} {
		r = mustParse(t, strings.Replace(debianFull, "info uid 1000\n",
			"info uid 1000 ubuntu\ninfo uid-home /home/ubuntu\ninfo uid-shell "+shell+"\n", 1))
		if r.SystemAccount() || len(r.Notes()) != 0 {
			t.Errorf("shell %q: a system account", shell)
		}
	}
	for _, shell := range []string{"/bin/false", "/sbin/nologin"} {
		r = mustParse(t, strings.Replace(debianFull, "info uid 1000\n", "info uid 1000 svc\ninfo uid-shell "+shell+"\n", 1))
		if !r.SystemAccount() {
			t.Errorf("shell %q: not a system account", shell)
		}
	}
}

// What layer-user.sh would die on, the check refuses first: the account
// files, and a /home/agent it cannot make agent's.
func TestUserSetupProblems(t *testing.T) {
	r := mustParse(t, strings.NewReplacer(
		"ok etc:passwd /etc/passwd", "missing etc:passwd no /etc/passwd",
		"ok home /home/agent (created by the layer)", "missing home /home/agent is a symlink").Replace(debianFull))
	ps := r.Problems()
	if len(ps) != 2 || !strings.HasPrefix(ps[0], "/etc/passwd: missing (no /etc/passwd) -- ") ||
		!strings.HasPrefix(ps[1], "/home/agent: missing (/home/agent is a symlink) -- ") {
		t.Errorf("problems %q", ps)
	}
	if !slices.Contains(r.Checklist(), Row{"user", "unusable: /etc/passwd (no /etc/passwd), /home/agent (/home/agent is a symlink)", Unmet}) {
		t.Errorf("checklist %v", r.Checklist())
	}
}

func TestNoShellReport(t *testing.T) {
	r := &Report{Image: "scratchy", NoShell: true}
	if r.OK() {
		t.Error("an image with no /bin/sh passed")
	}
	if ps := r.Problems(); len(ps) != 1 || !strings.HasPrefix(ps[0], "/bin/sh: missing -- ") ||
		!strings.Contains(ps[0], "nothing else could be checked") {
		t.Errorf("problems %q", ps)
	}
	if rows := r.Checklist(); !slices.Equal(rows, []Row{{"/bin/sh", "missing", Unmet}}) {
		t.Errorf("checklist %v", rows)
	}
}

// Docker's engine is a fact, never a requirement: an image without it, or
// with only part of it, is as usable as one with all of it. The row says
// which, and a report that never mentioned the engine has no row.
func TestDockerInside(t *testing.T) {
	engine := []string{
		"info engine dockerd /usr/local/bin/dockerd", "info engine containerd /usr/local/bin/containerd",
		"info engine containerd-shim-runc-v2 /usr/local/bin/containerd-shim-runc-v2",
		"info engine runc /usr/local/bin/runc", "info engine iptables /usr/sbin/iptables",
		"info engine docker /usr/local/bin/docker", "info engine-version 28.3.0",
	}
	with := func(lines ...string) *Report {
		return mustParse(t, strings.Replace(debianFull, "end\n", strings.Join(lines, "\n")+"\nend\n", 1))
	}
	none := []string{"info engine dockerd", "info engine containerd", "info engine containerd-shim-runc-v2",
		"info engine runc", "info engine iptables", "info engine docker"}
	noIptables := slices.Clone(engine)
	noIptables[4] = "info engine iptables"
	noCLI := slices.Clone(engine[:5])
	for _, tc := range []struct {
		name  string
		r     *Report
		value string
		level Level
		note  bool
	}{
		{"all", with(engine...), "available (dockerd 28.3.0)", Met, false},
		{"none", with(none...), "not in this image: `docker` won't work inside the sandbox", Noted, true},
		{"no iptables", with(noIptables...), "dockerd 28.3.0, but no iptables: it won't start, so `docker` won't work inside the sandbox", Noted, true},
		{"no cli", with(append(noCLI, "info engine docker", "info engine-version 28.3.0")...), "dockerd 28.3.0, but no docker CLI to reach it", Noted, true},
		{"no version", with(engine[:6]...), "available (dockerd)", Met, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !tc.r.OK() {
				t.Errorf("problems %q", tc.r.Problems())
			}
			row, ok := tc.r.DockerInside()
			if !ok || row.Label != "docker inside" || row.Value != tc.value || row.Level != tc.level {
				t.Errorf("row %+v (%v), want %q at %v", row, ok, tc.value, tc.level)
			}
			if note := tc.r.DockerInsideNote(); (note != "") != tc.note {
				t.Errorf("note %q", note)
			}
			for _, r := range tc.r.Checklist() {
				if r.Label == "docker inside" {
					t.Error("the checklist itself has the row; it is vm's alone")
				}
			}
		})
	}
	if note := with(noIptables...).DockerInsideNote(); !strings.Contains(note, "add iptables to the image") {
		t.Errorf("note does not name what is missing: %q", note)
	}
	if _, ok := mustParse(t, debianFull).DockerInside(); ok {
		t.Error("a row for a report that never mentioned the engine")
	}
	if _, ok := NoShellReport("x", 1000, 1000).DockerInside(); ok {
		t.Error("a row for an image the probe could not run in")
	}
}
