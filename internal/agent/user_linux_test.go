package agent

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"github.com/bfreis/caboose/internal/agentproto"
)

const testPasswd = `root:x:0:0:root:/root:/bin/sh
# a comment
+nis
bad line
short:x:5
nogid:x:6:x:::
agent:x:1000:1000::/home/agent:/bin/bash
dup:x:1000:7::/:/bin/sh
spaced:x:1001:1001::/:/bin/sh
`

const testGroup = `root:x:0:root
bin:x:1:root,bin,daemon
wheel:x:10:root, agent
docker:x:999:agent
docker2:x:999:agent
users:x:100:
# staff:x:50:agent
-nis:x:51:agent
bad:x:notanumber:agent
huge:x:4294967296:agent
short:x:52
agents:x:1001:agentx,xagent
`

func TestPasswdEntry(t *testing.T) {
	for _, tc := range []struct {
		uid  uint32
		name string
		gid  uint32
		ok   bool
	}{
		{0, "root", 0, true},
		{1000, "agent", 1000, true}, // the first entry wins
		{1001, "spaced", 1001, true},
		{5, "", 0, false}, // too few fields
		{6, "", 0, false}, // no GID
		{4242, "", 0, false},
	} {
		name, gid, ok := passwdEntry([]byte(testPasswd), tc.uid)
		if name != tc.name || gid != tc.gid || ok != tc.ok {
			t.Errorf("uid %d: %q %d %v", tc.uid, name, gid, ok)
		}
	}
	if _, _, ok := passwdEntry(nil, 0); ok {
		t.Error("an entry in no file")
	}
}

func TestMemberGroups(t *testing.T) {
	for name, want := range map[string][]uint32{
		"root":   {0, 1, 10},
		"agent":  {10, 999, 999},
		"daemon": {1},
		"agentx": {1001},
		"nobody": nil,
		"":       nil, // an empty member list names no one
	} {
		if got := memberGroups([]byte(testGroup), name); !reflect.DeepEqual(got, want) {
			t.Errorf("%q: %v, want %v", name, got, want)
		}
	}
}

func TestUserGroups(t *testing.T) {
	for _, tc := range []struct {
		name     string
		uid, gid uint32
		explicit bool
		wantGID  uint32
		want     []uint32
	}{
		{"root alone gets root's groups", 0, 0, false, 0, []uint32{0, 1, 10}},
		{"a user gets its passwd group and member groups, once each", 1000, 1000, false, 1000, []uint32{1000, 10, 999}},
		{"a group asked for gets no member groups", 1000, 5, true, 5, []uint32{5}},
		{"root:root too", 0, 0, true, 0, []uint32{0}},
		{"passwd's group over the UID", 1001, 1001, false, 1001, []uint32{1001}},
		{"a UID no entry names", 4242, 4242, false, 4242, []uint32{4242}},
	} {
		gid, groups := userGroups([]byte(testPasswd), []byte(testGroup), tc.uid, tc.gid, tc.explicit)
		if gid != tc.wantGID || !reflect.DeepEqual(groups, tc.want) {
			t.Errorf("%s: %d %v, want %d %v", tc.name, gid, groups, tc.wantGID, tc.want)
		}
	}
}

func accountRoot(t *testing.T, passwd, group string) string {
	t.Helper()
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "etc"), 0o755)
	if passwd != "" {
		os.WriteFile(filepath.Join(root, "etc", "passwd"), []byte(passwd), 0o644)
	}
	if group != "" {
		os.WriteFile(filepath.Join(root, "etc", "group"), []byte(group), 0o644)
	}
	return root
}

func TestUserCred(t *testing.T) {
	root := accountRoot(t, testPasswd, testGroup)
	for u, want := range map[string]syscall.Credential{
		"0":         {Uid: 0, Gid: 0, Groups: []uint32{0, 1, 10}},
		"1000":      {Uid: 1000, Gid: 1000, Groups: []uint32{1000, 10, 999}},
		"1000:1000": {Uid: 1000, Gid: 1000, Groups: []uint32{1000}},
		"0:0":       {Uid: 0, Gid: 0, Groups: []uint32{0}},
	} {
		c, err := userCred(root, u)
		if err != nil || !reflect.DeepEqual(*c, want) {
			t.Errorf("%s: %+v %v", u, c, err)
		}
	}
	if c, err := userCred(root, ""); c != nil || err != nil {
		t.Errorf("no user: %+v %v", c, err)
	}
	if _, err := userCred(root, "agent"); err == nil {
		t.Error("a name taken for a UID")
	}

	// No account files: the UID's own group, as docker has it.
	c, err := userCred(accountRoot(t, "", ""), "1000")
	if err != nil || c.Gid != 1000 || !reflect.DeepEqual(c.Groups, []uint32{1000}) {
		t.Errorf("no files: %+v %v", c, err)
	}
}

func TestReadAccountFile(t *testing.T) {
	root := accountRoot(t, "", "")
	etc := filepath.Join(root, "etc")

	// A link out of the root is refused, not followed.
	outside := filepath.Join(t.TempDir(), "group")
	os.WriteFile(outside, []byte(testGroup), 0o644)
	os.Symlink(outside, filepath.Join(etc, "group"))
	if b, err := readAccountFile(root, "etc/group"); err == nil {
		t.Errorf("followed a link out of the root: %q", b)
	}

	// One inside it is fine.
	os.WriteFile(filepath.Join(root, "passwd.real"), []byte(testPasswd), 0o644)
	os.Symlink("../passwd.real", filepath.Join(etc, "passwd"))
	if b, err := readAccountFile(root, "etc/passwd"); err != nil || string(b) != testPasswd {
		t.Errorf("link inside the root: %q %v", b, err)
	}

	// Not a file: refused rather than read.
	os.Remove(filepath.Join(etc, "group"))
	if err := syscall.Mkfifo(filepath.Join(etc, "group"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readAccountFile(root, "etc/group"); err == nil {
		t.Error("read a fifo")
	}

	// Past the bound, the file ends at its last whole line.
	os.Remove(filepath.Join(etc, "group"))
	line := "g:x:7:agent\n"
	big := strings.Repeat(line, maxAccountFile/len(line)+10)
	os.WriteFile(filepath.Join(etc, "group"), []byte(big), 0o644)
	b, err := readAccountFile(root, "etc/group")
	if err != nil || len(b) > maxAccountFile || len(b)%len(line) != 0 {
		t.Errorf("big file: %d bytes, %v", len(b), err)
	}
}

// A command the exec port runs has the groups its user has in the root.
func TestExecGroups(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("needs root to set groups")
	}
	root := accountRoot(t, testPasswd, testGroup)
	e := &ExecServer{Env: []string{"PATH=" + os.Getenv("PATH")}, Dir: "/", User: "0", Root: root}
	r := runExec(t, e, agentproto.ExecRequest{Argv: []string{"id", "-G"}}, "", nil)
	if r.code != 0 || strings.Fields(r.out)[0] != "0" || !strings.Contains(" "+r.out, " 10") || !strings.Contains(" "+r.out, " 1 ") {
		t.Errorf("as 0: %+v", r)
	}
	r = runExec(t, e, agentproto.ExecRequest{Argv: []string{"id", "-G"}, User: "0:0"}, "", nil)
	if r.code != 0 || strings.TrimSpace(r.out) != "0" {
		t.Errorf("as 0:0: %+v", r)
	}
}
