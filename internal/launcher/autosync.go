package launcher

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/bfreis/caboose/internal/backend"
	"github.com/bfreis/caboose/internal/statesync"
	"github.com/bfreis/caboose/internal/syncagent"
)

// syncBudget is how long an automatic sync may spend, all told, on what
// reaches the remote: the most a slow or unreachable remote adds to a
// launch. A variable for the tests.
var syncBudget = 20 * time.Second

// syncMargin is what a sync nobody watches may take beyond its budget for
// the remote, for its local work, before the host stops it: it never holds
// up a launch, or doctor, for good. A variable for the tests.
var syncMargin = 20 * time.Second

// slowSync is how long an automatic sync goes before it says it is running.
// A variable for the tests.
var slowSync = 2 * time.Second

// lockPoll is how often a launch looks again at a sync lock someone holds.
const lockPoll = 200 * time.Millisecond

// beforeAttach runs just before a launch starts Claude Code in the
// container: the one moment nothing there can be writing the home. With
// auto_sync set, and a terminal to launch on (a script's `caboose claude -p`
// is left alone: it would pay a round trip per call), it syncs; either
// way, a sync another caboose is running is waited for, so Claude Code
// never starts under one that is writing the home.
//
// Nothing here fails the launch: whatever goes wrong is one note, and
// `caboose sync` is how to see to it.
func (a *App) beforeAttach(terminal bool) {
	if a.Cfg.AutoSync && terminal {
		a.autoSync()
		return
	}
	if unlock, ok := a.lockSync(); ok {
		unlock()
	}
}

// lockSync takes the sync lock, waiting up to syncBudget for a sync that
// holds it. ok is false when it could not be had; waited is said once.
func (a *App) lockSync() (unlock func(), ok bool) {
	deadline := time.Now().Add(syncBudget)
	said := false
	for {
		unlock, err := statesync.Lock(a.Cfg.DataDir)
		switch {
		case err == nil:
			return unlock, true
		case !errors.Is(err, statesync.ErrLocked):
			return nil, false // no lock file to be had: nobody syncs here either
		case time.Now().After(deadline):
			a.Note("a sync is still running; starting anyway")
			return nil, false
		case !said:
			a.Note("waiting for a sync to finish")
			said = true
		}
		time.Sleep(lockPoll)
	}
}

// budgetSecs is d in whole seconds, at least one.
func budgetSecs(d time.Duration) int {
	return max(int((d+time.Second-1)/time.Second), 1)
}

// autoSync is the launch's sync; see beforeAttach.
func (a *App) autoSync() {
	if !statesync.MaybeRemote(a.syncRepo()) {
		return // no remote: nothing to sync with, and nothing to say
	}
	// The lock first: a sync that was running when this launch came up
	// has just done this one's work.
	start := time.Now()
	unlock, ok := a.lockSync()
	if !ok {
		return
	}
	defer unlock()
	if time.Since(start) >= lockPoll {
		return
	}
	if l, err := a.liveWork(); err != nil {
		a.notSynced(err.Error())
		return
	} else if !l.none() {
		return // not the moment: something is writing the home
	}
	mounts, err := a.checkSyncMount()
	if err != nil {
		a.notSynced("the " + a.noun() + " has no sync mount for this data dir; 'caboose restart' gives it one")
		return
	}
	req, err := a.syncRequest(mounts)
	if err != nil {
		a.notSynced(err.Error())
		return
	}
	req.Auto, req.Budget = true, budgetSecs(syncBudget)

	var (
		mu   sync.Mutex
		done bool
	)
	slow := time.AfterFunc(slowSync, func() {
		mu.Lock()
		defer mu.Unlock()
		if !done {
			a.Note("syncing with the remote")
		}
	})
	cmd := a.box().Command(backend.ExecSpec{Argv: []string{AgentPath, "sync", syncagent.OpRun}, Stdin: true})
	res, err := syncagent.Run(cmd, req, syncagent.Handler{Timeout: syncBudget + syncMargin}, nil)
	mu.Lock()
	done = true
	mu.Unlock()
	slow.Stop()

	switch {
	case errors.Is(err, syncagent.ErrOldAgent):
		a.notSynced(a.oldAgent() + "; 'caboose restart' recreates it")
	case err != nil:
		a.notSynced(firstLine(shown(err.Error())) + "; run 'caboose sync' to see to it")
	case res.Err != nil && res.Err.Kind == syncagent.KindAborted:
		a.notSynced(shown(strings.TrimPrefix(res.Err.Message, statesync.ErrAborted.Error()+": ")) + "; run 'caboose sync' to settle it")
	case res.Err != nil:
		a.notSynced(firstLine(shown(res.Err.Message)) + "; run 'caboose sync' to see to it")
	default:
		if res.Report == nil {
			return
		}
		if line := syncedLine(res.Report); line != "" {
			a.Note("%s", line)
		}
		if len(res.Report.Unmounted) > 0 {
			a.Note("%s not synced: the %s does not mount %s yet; 'caboose restart' does",
				homeList(res.Report.Unmounted), a.noun(), plural(len(res.Report.Unmounted), "it", "them"))
		}
		a.afterSync(res.Report)
	}
}

// notSynced is the one line an automatic sync that did not happen says.
func (a *App) notSynced(why string) {
	a.Note("not synced: %s", why)
}

// syncedLine is what an automatic sync that did something says, in one
// line; "" when there was nothing to do.
func syncedLine(r *statesync.Report) string {
	var did []string
	if r.Merged {
		did = append(did, fmt.Sprintf("took %d %s from the remote", len(r.Applied), plural(len(r.Applied), "change", "changes")))
	}
	if r.Committed && r.Pushed {
		did = append(did, "sent this machine's")
	}
	if len(did) == 0 {
		return ""
	}
	return "synced: " + strings.Join(did, ", ")
}

// firstLine is s's first line, without the \r ssh ends each with.
func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return strings.TrimRight(line, "\r")
}
