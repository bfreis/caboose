package agent

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// A process docker exec (or docker run) starts gets its user's groups
// from the container's /etc/passwd and /etc/group, as login(1) would
// give them: a vm guest has no docker to do it, so without this a
// command run through the exec port had no supplementary groups at all,
// and a user in, say, a group that owns a socket could not use it there
// as it could in a container. userCred does what moby does (its getUser,
// over moby/sys/user's GetExecUser), at every exec as moby does, so a
// group the sandbox adds is seen by the next command:
//
//   - "UID": the user's name is the passwd entry's with that UID, its
//     group the entry's GID, and its supplementary groups every group
//     that lists that name as a member. Root is no exception.
//   - "UID:GID": the group asked for, and no member groups: docker
//     exec -u UID:GID gives none either.
//
// Either way the primary group is in the list as well, as moby puts it
// there (since CVE-2022-36109), and a UID no entry names keeps this
// agent's rule of its UID as its GID (parseUser).

// maxAccountFile is the most of /etc/passwd or /etc/group read: far past
// any real one, and a bound on what a bad image costs the agent.
const maxAccountFile = 4 << 20

// userCred is who u runs as on the system at root: parseUser's UID and
// GID, and the groups root's account files give it. No user is nil: the
// agent's own.
func userCred(root, u string) (*syscall.Credential, error) {
	if u == "" {
		return nil, nil
	}
	cred, err := parseUser(u)
	if err != nil {
		return nil, err
	}
	_, _, explicitGID := strings.Cut(u, ":")
	var passwd, group []byte
	if !explicitGID {
		if passwd, err = readAccountFile(root, "etc/passwd"); err != nil {
			return nil, err
		}
		if group, err = readAccountFile(root, "etc/group"); err != nil {
			return nil, err
		}
	}
	cred.Gid, cred.Groups = userGroups(passwd, group, cred.Uid, cred.Gid, explicitGID)
	return cred, nil
}

// userGroups is the primary group and the supplementary groups of uid,
// whose GID is gid unless passwd says otherwise and explicitGID is not
// set; the primary group comes first, and none twice.
func userGroups(passwd, group []byte, uid, gid uint32, explicitGID bool) (uint32, []uint32) {
	groups := []uint32{}
	add := func(g uint32) {
		for _, h := range groups {
			if h == g {
				return
			}
		}
		groups = append(groups, g)
	}
	if explicitGID {
		add(gid)
		return gid, groups
	}
	name, pgid, ok := passwdEntry(passwd, uid)
	if ok {
		gid = pgid
	}
	add(gid)
	if ok {
		for _, g := range memberGroups(group, name) {
			add(g)
		}
	}
	return gid, groups
}

// accountLines calls fn with the fields of each line of an account file
// that is neither blank nor a comment, nor a NIS "+"/"-" entry.
func accountLines(b []byte, fn func(fields []string)) {
	for line := range bytes.Lines(b) {
		l := strings.TrimSpace(string(line))
		if l == "" || l[0] == '#' || l[0] == '+' || l[0] == '-' {
			continue
		}
		fn(strings.Split(l, ":"))
	}
}

// passwdEntry is the name and GID of the first passwd entry for uid, as
// getpwuid finds it; a malformed line is skipped.
func passwdEntry(passwd []byte, uid uint32) (name string, gid uint32, ok bool) {
	accountLines(passwd, func(f []string) {
		if ok || len(f) < 4 {
			return
		}
		u, err := parseID(f[2])
		if err != nil || u != uid {
			return
		}
		g, err := parseID(f[3])
		if err != nil {
			return
		}
		name, gid, ok = f[0], g, true
	})
	return name, gid, ok
}

// memberGroups is the GID of every group that lists name as a member,
// in the order /etc/group has them; a malformed line is skipped.
func memberGroups(group []byte, name string) []uint32 {
	var gids []uint32
	if name == "" {
		return nil
	}
	accountLines(group, func(f []string) {
		if len(f) < 4 {
			return
		}
		g, err := parseID(f[2])
		if err != nil {
			return
		}
		for _, m := range strings.Split(f[3], ",") {
			if strings.TrimSpace(m) == name {
				gids = append(gids, g)
				return
			}
		}
	})
	return gids
}

func parseID(s string) (uint32, error) {
	n, err := strconv.ParseUint(strings.TrimSpace(s), 10, 32)
	return uint32(n), err
}

// readAccountFile reads name under root, through os.Root, so that a link
// cannot take it outside root (in the guest root is "/", where it cannot
// go anywhere, but the exec server takes any root). A missing file is an
// empty one, as it is to docker; past maxAccountFile, the file ends at
// the last whole line before it.
func readAccountFile(root, name string) ([]byte, error) {
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	st, err := r.Stat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		// A fifo would hold the exec forever.
		return nil, fmt.Errorf("/%s is not a file", name)
	}
	f, err := r.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxAccountFile+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxAccountFile {
		b = b[:bytes.LastIndexByte(b[:maxAccountFile], '\n')+1]
	}
	return b, nil
}
