package vm

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/bfreis/caboose/internal/agentproto"
	"github.com/bfreis/caboose/internal/hvsock"
	"github.com/bfreis/caboose/internal/tty"
)

// A command in a vm guest is a host process, as `docker exec` is: sync's
// git runs one, and the attach replaces the launcher with one. It is the
// launcher again, run as ExecHelper, a client of the guest's exec port.
// What to run comes in the environment, execEnv, not in its arguments,
// which anyone on the host can read.
const (
	ExecHelper = "__vm-exec"
	execEnv    = "CABOOSE_VM_EXEC"
)

// ExecFailed is the helper's status when it could not run the command at
// all, as docker's own failures are 125.
const ExecFailed = 125

// dialTimeout bounds reaching the exec port.
const dialTimeout = 10 * time.Second

type execJob struct {
	Socket string                 `json:"socket"`
	Req    agentproto.ExecRequest `json:"req"`
}

// ExecCommand is self, this launcher, as the helper that runs req in the
// guest behind socket; its stdio are the caller's to connect.
func ExecCommand(self, socket string, req agentproto.ExecRequest) *exec.Cmd {
	cmd := exec.Command(self, ExecHelper)
	b, err := json.Marshal(execJob{Socket: socket, Req: req})
	if err != nil {
		cmd.Err = err
		return cmd
	}
	cmd.Env = append(os.Environ(), execEnv+"="+string(b))
	return cmd
}

// ExecMain is the helper: it runs the command its environment names and
// returns the command's status, or ExecFailed.
func ExecMain(stdin *os.File, stdout, stderr io.Writer) int {
	var job execJob
	if err := json.Unmarshal([]byte(os.Getenv(execEnv)), &job); err != nil || len(job.Req.Argv) == 0 {
		fmt.Fprintf(stderr, "caboose: %s is run by caboose itself, with a command to run in a vm\n", ExecHelper)
		return ExecFailed
	}
	conn, err := hvsock.Dial(job.Socket, agentproto.PortExec, dialTimeout)
	if err != nil {
		fmt.Fprintf(stderr, "caboose: cannot reach the sandbox's VM: %v\n", err)
		return ExecFailed
	}
	stdio := agentproto.ExecIO{Stdin: stdin, Stdout: stdout, Stderr: stderr}
	req := job.Req
	if req.TTY && tty.IsTerminal(stdin.Fd()) {
		fd := stdin.Fd()
		req.Rows, req.Cols = tty.Size(fd)
		restore, err := tty.MakeRaw(fd)
		if err != nil {
			fmt.Fprintf(stderr, "caboose: cannot put the terminal in raw mode: %v\n", err)
			conn.Close()
			return ExecFailed
		}
		defer restore()
		stdio.Resize = resizes(fd)
	}
	code, err := agentproto.Exec(conn, req, stdio)
	if err != nil {
		fmt.Fprintf(stderr, "caboose: %v\n", err)
		return ExecFailed
	}
	return code
}

// resizes sends the terminal's size whenever it changes.
func resizes(fd uintptr) <-chan [2]uint16 {
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	out := make(chan [2]uint16, 1)
	go func() {
		for range winch {
			r, c := tty.Size(fd)
			select {
			case out <- [2]uint16{r, c}:
			default:
				// One pending is enough: the next says the latest.
				select {
				case <-out:
				default:
				}
				out <- [2]uint16{r, c}
			}
		}
	}()
	return out
}
