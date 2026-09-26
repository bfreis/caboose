package statesync

import (
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"

	"github.com/bfreis/caboose/internal/nofollow"
)

// Pending is what this machine has changed since its last sync: the live
// export against the sync repo's work tree, which every sync leaves at the
// commit it made or took (clean resets it at the start, a failed merge is
// aborted). Read on the host, with no git and no network.
type Pending struct {
	// Changed are repo paths the export has and the work tree lacks, or
	// holds with other contents or another mode.
	Changed []string
	// Deleted are repo paths the work tree holds that this machine syncs,
	// and whose live source is gone.
	Deleted []string
	// Export is the export compared: its Refused and Shadowed, and
	// ScanSecrets, say what a sync would make of it.
	Export *Export
}

// Any reports whether anything is waiting to be sent.
func (p *Pending) Any() bool { return len(p.Changed)+len(p.Deleted) > 0 }

// Pending compares the live data dir with the sync repo's work tree. The
// caller holds the lock, so no sync is rewriting the work tree meanwhile.
// Both are read without following a link: a work tree path that is not a
// plain file reads as changed, as the next sync rewrites it.
func (s *Syncer) Pending() (*Pending, error) {
	c, err := s.rules()
	if err != nil {
		return nil, err
	}
	e, err := ExportLive(s.DataDir, c)
	if err != nil {
		return nil, err
	}
	prev := s.prevRules()
	p := &Pending{Export: e}
	repo := s.repo()
	for path, f := range e.Files {
		data, mode, err := repo.ReadFile(path)
		switch {
		case errors.Is(err, fs.ErrNotExist):
		case err != nil && !errors.Is(err, nofollow.ErrNotPlain):
			return nil, err
		case err == nil && string(data) == string(f.Data) && mode&0o100 != 0 == f.Exec:
			continue
		}
		p.Changed = append(p.Changed, path)
	}
	err = repo.Walk("", func(path string, ent fs.DirEntry) error {
		if path == ".git" {
			return fs.SkipDir
		}
		if ent.IsDir() {
			return nil
		}
		if _, ok := e.Files[path]; ok || e.kept(path) {
			return nil
		}
		// Only what this machine synced last time too can read as
		// deleted, as commitExport has it.
		if !deletable(c, prev, path) {
			return nil
		}
		p.Deleted = append(p.Deleted, path)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(p.Changed)
	sort.Strings(p.Deleted)
	return p, nil
}

// Dirty reports whether the work tree differs from HEAD: a sync that died
// between writing it and committing, which the next one finishes off
// (clean). Pending is then measured against a tree no commit holds. No
// network.
func (s *Syncer) Dirty() (bool, error) {
	out, err := s.git().out("status", "--porcelain", "-z", "--untracked-files=all")
	if err != nil {
		return false, err
	}
	return len(out) > 0, nil
}

// Fetch brings the remote's branch into refs/remotes/origin, as a sync's
// first step does; it changes nothing a sync depends on not changing.
func (s *Syncer) Fetch() error {
	return s.git().talk("fetch", "-q", "origin")
}

// Divergence is how HEAD, the last sync, stands against the remote branch
// as last fetched.
type Divergence struct {
	// Remote is set when the remote has the branch at all: an empty remote
	// is one no machine has pushed to yet.
	Remote bool
	// Ahead counts the commits HEAD has that the remote lacks: a sync
	// whose push did not get through. Behind counts the remote's that HEAD
	// lacks: changes not taken yet.
	Ahead, Behind int
}

// Divergence compares HEAD with the remote branch as the last fetch left
// it. No network: Fetch first for an answer that is current.
func (s *Syncer) Divergence() (Divergence, error) {
	g := s.git()
	remote := "refs/remotes/origin/" + Branch
	if has, err := g.ok("rev-parse", "-q", "--verify", remote); err != nil || !has {
		return Divergence{}, err
	}
	out, err := g.str("rev-list", "--left-right", "--count", "HEAD..."+remote)
	if err != nil {
		return Divergence{}, err
	}
	f := strings.Fields(out)
	if len(f) != 2 {
		return Divergence{}, fmt.Errorf("git rev-list --count: unexpected %q", out)
	}
	ahead, err1 := strconv.Atoi(f[0])
	behind, err2 := strconv.Atoi(f[1])
	if err1 != nil || err2 != nil {
		return Divergence{}, fmt.Errorf("git rev-list --count: unexpected %q", out)
	}
	return Divergence{Remote: true, Ahead: ahead, Behind: behind}, nil
}

// Incoming lists what the remote branch, as last fetched, has changed since
// HEAD and it parted: taken, the repo paths this machine's rules sync;
// others, the ones they do not (another machine's rules, a newer
// caboose's), which a sync leaves in the repo. No network: Fetch first.
func (s *Syncer) Incoming() (taken, others []string, err error) {
	c, err := s.rules()
	if err != nil {
		return nil, nil, err
	}
	g := s.git()
	remote := "refs/remotes/origin/" + Branch
	if has, err := g.ok("rev-parse", "-q", "--verify", remote); err != nil || !has {
		return nil, nil, err
	}
	out, err := g.out("diff", "--no-ext-diff", "--no-textconv", "--no-renames", "--name-only", "-z", "HEAD..."+remote)
	if err != nil {
		return nil, nil, err
	}
	for _, p := range zsplit(out) {
		if _, err := LiveTarget(c, p); err == nil {
			taken = append(taken, p)
		} else {
			others = append(others, p)
		}
	}
	return taken, others, nil
}
