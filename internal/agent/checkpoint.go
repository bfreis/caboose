package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"time"

	"github.com/bfreis/caboose/internal/agentproto"
)

// The exit statuses of `caboose-agent checkpoint` beyond 0 (every path
// taken), 1 (some not) and 2 (usage, or a path the host refuses): the
// host's policy refuses it (checkpoint_requests = "off", EX_NOPERM, as
// run's 77), it was asked too often or another request of the link's is
// still being taken (EX_TEMPFAIL: try again later), or the host takes
// none (none offered, none configured, the store unreachable, no link:
// EX_UNAVAILABLE).
const (
	ExitCheckpointDenied      = ExitDenied
	ExitCheckpointLimit       = 75
	ExitCheckpointUnavailable = 69
)

// checkpointsOff is what a checkpoint request gets from a host whose
// hello offered none.
const checkpointsOff = "the host takes no checkpoints for this sandbox: its caboose is older than this sandbox's, " +
	"or its link is not the host daemon's (checkpoints are [checkpoints] paths in config.toml, on the host)"

// checkpointRequest asks the host for a checkpoint of paths now, waiting
// for the takes a little longer than the host spends on them.
func (l *Link) checkpointRequest(paths []string) agentproto.Message {
	l.waitHelloed()
	l.mu.Lock()
	on := l.checkpointsOn
	l.mu.Unlock()
	if !on {
		return agentproto.Message{Type: agentproto.TypeResponse, Error: checkpointsOff, Reason: agentproto.CheckpointUnavailable}
	}
	if err := checkCheckpointArgs(paths); err != nil {
		return agentproto.Message{Type: agentproto.TypeResponse, Error: err.Error(), Reason: agentproto.CheckpointBad}
	}
	return l.requestCtx(context.Background(), agentproto.Message{Op: agentproto.OpCheckpoint, Paths: paths}, agentproto.CheckpointWait+30*time.Second)
}

// checkCheckpointArgs refuses what the host would: too many paths, one too
// long, unprintable or not a container path (absolute, or ~/...).
func checkCheckpointArgs(paths []string) error {
	if len(paths) > agentproto.MaxCheckpointPaths {
		return fmt.Errorf("%d paths, at most %d", len(paths), agentproto.MaxCheckpointPaths)
	}
	for _, p := range paths {
		switch {
		case len(p) > agentproto.MaxCheckpointPath:
			return fmt.Errorf("a path of %d bytes is too long (at most %d)", len(p), agentproto.MaxCheckpointPath)
		case printable(p) != p:
			return fmt.Errorf("path %q has a control or invisible character", printable(p))
		case !strings.HasPrefix(p, "/") && !strings.HasPrefix(p, "~/"):
			return fmt.Errorf("%q is not a container path: absolute, or ~/... for one under the home", p)
		}
	}
	return nil
}

// cleanCheckpointArg is p as it is sent: an absolute path cleaned, a
// trailing slash dropped; anything else as it is, for the checks.
func cleanCheckpointArg(p string) string {
	switch {
	case strings.HasPrefix(p, "/"):
		return path.Clean(p)
	case strings.HasPrefix(p, "~/") && len(p) > 2:
		return "~/" + strings.TrimPrefix(path.Clean(p[2:]), "/")
	}
	return p
}

// Checkpoint asks the host, through the link on socket, for a checkpoint
// of paths now (none: every configured one), and returns what it took.
func Checkpoint(socket string, paths []string) ([]agentproto.CheckpointTaken, error) {
	r, err := ask(socket, agentproto.Message{Op: agentproto.OpCheckpoint, Paths: paths})
	if err != nil {
		if r.Reason != "" {
			return nil, &Refused{Reason: r.Reason, Msg: err.Error()}
		}
		return nil, err
	}
	return r.Checkpoints, nil
}

// checkpointCommand is `caboose-agent checkpoint [--json] [PATH...]`.
func checkpointCommand(args []string, stdout, stderr io.Writer, usage func() int) int {
	return checkpointWith(linkSocket, args, stdout, stderr, usage)
}

// checkpointWith is checkpointCommand through the link on socket.
func checkpointWith(socket string, args []string, stdout, stderr io.Writer, usage func() int) int {
	asJSON := false
	var paths []string
	for i, a := range args {
		if a == "--" {
			for _, p := range args[i+1:] {
				paths = append(paths, cleanCheckpointArg(p))
			}
			break
		}
		if a == "--json" {
			asJSON = true
			continue
		}
		if strings.HasPrefix(a, "-") {
			return usage()
		}
		paths = append(paths, cleanCheckpointArg(a))
	}
	if err := checkCheckpointArgs(paths); err != nil {
		fmt.Fprintf(stderr, "caboose-agent: checkpoint: %s\n", printable(err.Error()))
		return 2
	}
	list, err := Checkpoint(socket, paths)
	if err != nil {
		fmt.Fprintf(stderr, "caboose-agent: checkpoint: %s\n", printable(err.Error()))
		var r *Refused
		if errors.As(err, &r) && r.Reason == agentproto.CheckpointOff {
			return ExitCheckpointDenied
		}
		if errors.As(err, &r) && r.Reason == agentproto.CheckpointBad {
			return 2
		}
		if errors.As(err, &r) && r.Reason == agentproto.CheckpointLimit {
			return ExitCheckpointLimit
		}
		return ExitCheckpointUnavailable
	}
	if asJSON {
		if list == nil {
			list = []agentproto.CheckpointTaken{}
		}
		b, _ := json.MarshalIndent(list, "", "  ")
		fmt.Fprintf(stdout, "%s\n", b)
	}
	code := 0
	for _, t := range list {
		if t.ID == "" {
			code = 1
		}
		if asJSON {
			continue
		}
		if t.ID != "" {
			fmt.Fprintf(stdout, "%s: checkpoint %s\n", printable(t.Path), printable(t.ID))
		} else {
			fmt.Fprintf(stdout, "%s: not taken: %s\n", printable(t.Path), printable(t.Reason))
		}
	}
	if len(list) == 0 {
		fmt.Fprintln(stderr, "caboose-agent: checkpoint: the host took none")
		return 1
	}
	return code
}
