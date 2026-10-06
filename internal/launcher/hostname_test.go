package launcher

import (
	"errors"
	"strings"
	"testing"

	"github.com/bfreis/caboose/internal/assets"
	"github.com/bfreis/caboose/internal/backend/backendtest"
	"github.com/bfreis/caboose/internal/config"
)

func TestHostnameOf(t *testing.T) {
	long := strings.Repeat("a", 80)
	for _, tc := range []struct{ env, host, want string }{
		{"default", "laptop", "caboose-laptop"},
		{"default", "Work-Mac.example.com", "caboose-work-mac"},
		{"default", "my_mac book", "caboose-my-mac-book"},
		{"default", "--a__b--", "caboose-a-b"},
		{"default", "", "caboose"},
		{"default", "...", "caboose"},
		{"default", long, "caboose-" + strings.Repeat("a", 55)},
		{"test", "laptop", "caboose-laptop-test"},
		{"my_env", "laptop", "caboose-laptop-my-env"},
		{"test", "", "caboose-test"},
		// The host part gives way, not the environment; its trailing - goes.
		{"test", strings.Repeat("a", 40) + "-bbbbbbbb", "caboose-" + strings.Repeat("a", 40) + "-bbbbbbbb-test"},
		{"test", long, "caboose-" + strings.Repeat("a", 50) + "-test"},
		{"test", strings.Repeat("a", 49) + "-" + long, "caboose-" + strings.Repeat("a", 49) + "-test"},
	} {
		got := hostnameOf(tc.env, tc.host)
		if got != tc.want || len(got) > 63 {
			t.Errorf("hostnameOf(%q, %q) = %q (%d), want %q", tc.env, tc.host, got, len(got), tc.want)
		}
	}
}

func TestMachineName(t *testing.T) {
	osName := func() (string, error) { return "dhcp-42.example.com", nil }
	scutil := func(out string, err error) func(string, ...string) ([]byte, error) {
		return func(name string, args ...string) ([]byte, error) {
			if name != "scutil" || strings.Join(args, " ") != "--get LocalHostName" {
				t.Errorf("ran %s %v", name, args)
			}
			return []byte(out), err
		}
	}
	never := func(string, ...string) ([]byte, error) {
		t.Error("scutil ran off a Mac")
		return nil, nil
	}
	for _, tc := range []struct {
		name string
		goos string
		run  func(string, ...string) ([]byte, error)
		want string
	}{
		{"mac prefers LocalHostName", "darwin", scutil("work-mac\n", nil), "work-mac"},
		{"mac, scutil fails", "darwin", scutil("", errors.New("exit 1")), "dhcp-42.example.com"},
		{"mac, scutil empty", "darwin", scutil("\n", nil), "dhcp-42.example.com"},
		{"linux", "linux", never, "dhcp-42.example.com"},
	} {
		if got := machineName(tc.goos, tc.run, osName); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
	if got := machineName("linux", never, func() (string, error) { return "", errors.New("no") }); got != "" {
		t.Errorf("no name: %q", got)
	}
}

func TestHostnameConfigWins(t *testing.T) {
	a := &App{Cfg: &config.Config{Env: "test"}, HostPart: func() string { return "laptop" }}
	if got := a.hostname(); got != "caboose-laptop-test" {
		t.Errorf("default: %q", got)
	}
	a.Cfg.Hostname = "mybox"
	if got := a.hostname(); got != "mybox" {
		t.Errorf("configured: %q", got)
	}
}

func TestSyncHost(t *testing.T) {
	a := &App{Cfg: &config.Config{}, HostPart: func() string { return "work-mac" }}
	if got := a.syncHost(); got != "work-mac" {
		t.Errorf("syncHost %q", got)
	}
}

// The container's label against the hostname: one with no label is not
// this caboose's, and drifts.
func TestHostnameDrift(t *testing.T) {
	label := func(v string) map[string]string { return map[string]string{assets.LabelHostname: v} }
	for _, tc := range []struct {
		name     string
		labels   map[string]string
		cfg      string
		drift    string
		wantNone bool
	}{
		{"same", label("caboose-laptop"), "", "", true},
		{"no label", map[string]string{}, "", "records no hostname", false},
		{"no labels at all, configured", nil, "caboose", "records no hostname", false},
		{"changed", label("caboose-old"), "", "created with hostname caboose-old; the configuration says caboose-laptop", false},
		{"configured", label("caboose-laptop"), "mybox", "created with hostname caboose-laptop; the configuration says mybox", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			box := &backendtest.Fake{Status: "running", SandboxLabels: tc.labels}
			a := &App{Cfg: &config.Config{Env: "default", Container: "box", Hostname: tc.cfg}, Backend: box,
				HostPart: func() string { return "laptop" }}
			got := a.hostnameDrift()
			if tc.wantNone != (got == "") || !strings.Contains(got, tc.drift) {
				t.Errorf("drift %q, want %q", got, tc.drift)
			}
		})
	}
}

// A new container gets the hostname in its Spec and in its label.
func TestCreateContainerHostname(t *testing.T) {
	b := newBoxApp(t, isolationContainer, &backendtest.Fake{})
	b.Cfg.Env = "test"
	if err := b.createContainer(false); err != nil {
		t.Fatalf("%v\n%s", err, b.errb)
	}
	spec, _ := b.box.Spec()
	if spec.Hostname != "caboose-laptop-test" {
		t.Errorf("hostname %q", spec.Hostname)
	}
	if !hasSeq(spec.Labels, assets.LabelHostname+"=caboose-laptop-test") {
		t.Errorf("labels: %q", spec.Labels)
	}
}
