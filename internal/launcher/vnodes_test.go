package launcher

import (
	"bytes"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
)

func TestMaxVnodesLow(t *testing.T) {
	for _, c := range []struct {
		cur  uint32
		err  error
		want bool
	}{
		{263168, nil, true},
		{wantMaxVnodes - 1, nil, true},
		{wantMaxVnodes, nil, false},
		{2 << 20, nil, false},
		{0, errNoVnodes, false},
		{0, errors.New("boom"), false},
	} {
		if got := maxVnodesLow(c.cur, c.err); got != c.want {
			t.Errorf("maxVnodesLow(%d, %v) = %v, want %v", c.cur, c.err, got, c.want)
		}
	}
}

func TestMaxVnodesCommands(t *testing.T) {
	cmds := maxVnodesCommands("/tmp/x.plist")
	var got []string
	for _, c := range cmds {
		got = append(got, strings.Join(c, " "))
	}
	want := []string{
		"install -m 0644 -o root -g wheel /tmp/x.plist /Library/LaunchDaemons/dev.caboose.maxvnodes.plist",
		"sysctl kern.maxvnodes=1048576",
		"launchctl bootstrap system /Library/LaunchDaemons/dev.caboose.maxvnodes.plist",
	}
	if !slices.Equal(got, want) {
		t.Errorf("commands = %q, want %q", got, want)
	}
	plist := maxVnodesPlistText()
	for _, s := range []string{"<string>dev.caboose.maxvnodes</string>", "<string>/usr/sbin/sysctl</string>",
		"<string>kern.maxvnodes=1048576</string>", "<key>RunAtLoad</key>\n\t<true/>"} {
		if !strings.Contains(plist, s) {
			t.Errorf("plist lacks %q:\n%s", s, plist)
		}
	}
}

// vnodesOffer runs offerMaxVnodes with kern.maxvnodes at cur (err when it
// cannot be read), answering answer, and returns what it said and the
// sudo commands it ran; sudoErr fails the command that starts with it.
func vnodesOffer(t *testing.T, iso string, cur uint32, err error, answer, sudoErr string) (said string, ran []string) {
	t.Helper()
	oldNow, oldSudo := maxVnodesNow, runSudo
	t.Cleanup(func() { maxVnodesNow, runSudo = oldNow, oldSudo })
	maxVnodesNow = func() (uint32, error) { return cur, err }
	runSudo = func(args ...string) error {
		ran = append(ran, strings.Join(args, " "))
		if sudoErr != "" && args[0] == sudoErr {
			return errors.New("exit status 1")
		}
		if args[0] == "install" {
			if _, err := os.Stat(args[len(args)-2]); err != nil {
				t.Errorf("plist not written before install: %v", err)
			}
		}
		return nil
	}
	var out bytes.Buffer
	a := newSetupEnv(t, "default", "", false).a
	if e := a.offerMaxVnodes(newPrompter(strings.NewReader(answer), &out), iso); e != nil {
		t.Fatal(e)
	}
	return out.String(), ran
}

func TestOfferMaxVnodes(t *testing.T) {
	said, ran := vnodesOffer(t, isolationVM, 263168, nil, "y\n", "")
	if len(ran) != 3 || !strings.HasPrefix(ran[0], "install ") || ran[1] != "sysctl kern.maxvnodes=1048576" || !strings.HasPrefix(ran[2], "launchctl bootstrap system ") {
		t.Errorf("ran %q", ran)
	}
	for _, s := range []string{"263168", maxVnodesPlist, "sudo sysctl kern.maxvnodes=1048576", "Run these through sudo"} {
		if !strings.Contains(said, s) {
			t.Errorf("output lacks %q:\n%s", s, said)
		}
	}

	said, ran = vnodesOffer(t, isolationVM, 263168, nil, "n\n", "")
	if len(ran) != 0 || !strings.Contains(said, "sudo sysctl kern.maxvnodes=1048576") || !strings.Contains(said, "Not changed") {
		t.Errorf("declined: ran %q\n%s", ran, said)
	}

	for name, c := range map[string]struct {
		iso string
		cur uint32
		err error
	}{
		"docker":      {isolationContainer, 1000, nil},
		"high enough": {isolationVM, wantMaxVnodes, nil},
		"set by hand": {isolationVM, 2097152, nil},
		"not a mac":   {isolationVM, 0, errNoVnodes},
	} {
		said, ran = vnodesOffer(t, c.iso, c.cur, c.err, "y\n", "")
		if said != "" || len(ran) != 0 {
			t.Errorf("%s: should do nothing, ran %q said %q", name, ran, said)
		}
	}

	said, ran = vnodesOffer(t, isolationVM, 263168, nil, "y\n", "sysctl")
	if len(ran) != 2 || !strings.Contains(said, "nothing more was run") || !strings.Contains(said, "sudo sysctl kern.maxvnodes=1048576") {
		t.Errorf("sysctl fails: ran %q\n%s", ran, said)
	}
	said, ran = vnodesOffer(t, isolationVM, 263168, nil, "y\n", "launchctl")
	if len(ran) != 3 || !strings.Contains(said, "kern.maxvnodes is raised, but") {
		t.Errorf("bootstrap fails: ran %q\n%s", ran, said)
	}
}

func TestDoctorMaxVnodes(t *testing.T) {
	old := maxVnodesNow
	t.Cleanup(func() { maxVnodesNow = old })
	a := newSetupEnv(t, "default", "", false).a
	for _, c := range []struct {
		cur  uint32
		err  error
		want string
	}{
		{263168, nil, "sudo sysctl kern.maxvnodes=1048576"},
		{wantMaxVnodes, nil, "kern.maxvnodes is 1048576"},
		{0, errNoVnodes, ""},
		{0, errors.New("boom"), "cannot read kern.maxvnodes"},
	} {
		maxVnodesNow = func() (uint32, error) { return c.cur, c.err }
		ck := &checkup{}
		a.doctorMaxVnodes(ck)
		var got string
		for _, r := range ck.rows {
			got += r.text + " | " + r.fix
		}
		if (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("cur %d err %v: %q, want %q", c.cur, c.err, got, c.want)
		}
	}
}
