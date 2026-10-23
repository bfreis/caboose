package launcher

import (
	"slices"
	"strings"
	"testing"

	"github.com/bfreis/caboose/internal/config"
)

// run_args may not publish onto the host's web UI ports, in any spelling
// docker reads, alone or within a range; other publishes pass.
func TestRunArgsRefusePublishOntoUIPorts(t *testing.T) {
	for _, arg := range []string{
		"--publish=7171:80", "--publish=127.0.0.1:7171:80", "--publish=7172:1", "--publish=0.0.0.0:7172:80/tcp",
		"--publish=7000-7999:7000-7999", "--publish=[::1]:7171:80", "--publish=::1:7171:80", "--publish=7171-7172:80-81/udp",
		"--publish=07171:80",
	} {
		err := checkRunArgs([]string{"--dns=1.1.1.1", arg}, testOwnArgs, nil)
		if err == nil || !strings.Contains(err.Error(), "web UI") || !strings.HasPrefix(err.Error(), "'"+arg+"'") {
			t.Errorf("%s: err = %v", arg, err)
		}
	}
	// Short and split forms are refused already, as every short flag is.
	for _, args := range [][]string{{"-p", "7171:80"}, {"-p", "127.0.0.1:7171:80"}, {"-p", "7000-7999:7000-7999"}, {"--publish", "7171:80"}} {
		if err := checkRunArgs(args, testOwnArgs, nil); err == nil {
			t.Errorf("%q: not refused", args)
		}
	}
	// What cannot be read is refused, not guessed at.
	for _, arg := range []string{"--publish=x:80", "--publish=[::1:7171:80", "--publish=8000-7000:80", "--publish=99999:80", "--publish=published=7171,target=80"} {
		if err := checkRunArgs([]string{arg}, testOwnArgs, nil); err == nil {
			t.Errorf("%s: not refused", arg)
		}
	}
	for _, arg := range []string{
		"--publish=8080:80", "--publish=127.0.0.1:8080:80", "--publish=7170:80", "--publish=7173-7999:80",
		"--publish=7171", "--publish=127.0.0.1::7171", "--publish=8080:7171/tcp", "--publish-all",
	} {
		if err := checkRunArgs([]string{arg}, testOwnArgs, nil); err != nil {
			t.Errorf("%s: %v", arg, err)
		}
	}
}

// The host's network is found in every spelling, and doctor says it as
// a problem naming the web UI.
func TestHostNetworkRunArgs(t *testing.T) {
	args := []string{"--network", "host", "--dns=1.1.1.1", "--net=host", "--network=bridge", "--net", "host", "--network=host", "--network host", "--net=hosts"}
	want := []string{"--network host", "--net=host", "--net host", "--network=host", "--network host"}
	if got := hostNetworkArgs(args); !slices.Equal(got, want) {
		t.Errorf("hostNetworkArgs = %q, want %q", got, want)
	}
	if got := wideningRunArgs(args); !slices.Equal(got, want) {
		t.Errorf("wideningRunArgs = %q, want %q", got, want)
	}
	for _, run := range [][]string{{"--network=host"}, {"--net=host"}, {"--network", "host"}, {"--net", "host"}, {"--cap-add=NET_ADMIN", "--network=host"}} {
		a := &App{Cfg: &config.Config{RunArgs: run, Profile: "container.default", Isolation: "container"}}
		c := &checkup{}
		a.doctorRunArgs(c)
		found := false
		for _, r := range c.rows {
			if r.level == levelProblem && strings.Contains(r.text, "host's network") && strings.Contains(r.text, "web UI") && strings.Contains(r.fix, "remove") {
				found = true
			}
			if r.level == levelNote && strings.Contains(r.text, "host") && !strings.Contains(r.text, "NET_ADMIN") {
				t.Errorf("%q: the host network noted too: %+v", run, r)
			}
		}
		if !found {
			t.Errorf("%q: no problem in %+v", run, c.rows)
		}
	}
	a := &App{Cfg: &config.Config{RunArgs: []string{"--network=bridge", "--cap-add=NET_ADMIN"}, Profile: "container.default", Isolation: "container"}}
	c := &checkup{}
	a.doctorRunArgs(c)
	for _, r := range c.rows {
		if r.level == levelProblem {
			t.Errorf("a problem without the host network: %+v", r)
		}
	}
}
