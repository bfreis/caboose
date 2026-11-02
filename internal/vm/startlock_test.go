package vm

import (
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// timeoutErr is a dial that timed out.
type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

// stubDial makes vm.sock's dials answer errs in turn, then succeed.
func stubDial(t *testing.T, errs ...error) *int {
	t.Helper()
	prev := dialVMM
	calls := 0
	dialVMM = func(string, time.Duration) (net.Conn, error) {
		calls++
		if calls <= len(errs) {
			return nil, errs[calls-1]
		}
		a, b := net.Pipe()
		b.Close()
		return a, nil
	}
	t.Cleanup(func() { dialVMM = prev })
	return &calls
}

// A dial that times out is tried again, with a longer wait: a vmm slow to
// accept still answers. No socket, or nothing listening, is a no at once.
func TestAnswers(t *testing.T) {
	d := Dir(t.TempDir())
	calls := stubDial(t, &net.OpError{Op: "dial", Err: timeoutErr{}})
	if !d.Answers() || *calls != 2 {
		t.Errorf("after a timeout: calls %d", *calls)
	}
	calls = stubDial(t, timeoutErr{}, timeoutErr{}, timeoutErr{})
	if d.Answers() || *calls != len(answerWaits) {
		t.Errorf("timing out every time: calls %d", *calls)
	}
	for _, e := range []error{syscall.ENOENT, syscall.ECONNREFUSED} {
		calls = stubDial(t, &net.OpError{Op: "dial", Err: os.NewSyscallError("connect", e)})
		if d.Answers() || *calls != 1 {
			t.Errorf("%v: calls %d", e, *calls)
		}
	}
}

// Answers against real sockets: none, one nothing listens on, one served.
func TestAnswersSockets(t *testing.T) {
	d := Dir(t.TempDir())
	if len(d.Socket()) > 100 {
		t.Skip("socket path too long here")
	}
	if d.Answers() {
		t.Error("no socket answers")
	}
	l, err := net.Listen("unix", d.Socket())
	if err != nil {
		t.Fatal(err)
	}
	if !d.Answers() {
		t.Error("served socket does not answer")
	}
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	l.Close()
	if d.Answers() {
		t.Error("stale socket answers")
	}
}

// Runs is a yes while another holds the vmm's lock, whatever vm.sock
// says, and asks vm.sock when none does.
func TestRuns(t *testing.T) {
	d := Dir(t.TempDir())
	stubDial(t, syscall.ENOENT, syscall.ENOENT)
	unlock, err := TryLock(d.Lock())
	if err != nil {
		t.Fatal(err)
	}
	if !d.Runs() {
		t.Error("lock held, but not running")
	}
	unlock()
	if d.Runs() {
		t.Error("no lock, no socket, but running")
	}
	stubDial(t)
	if !d.Runs() {
		t.Error("no lock, but answering: not running")
	}
	if _, err := os.Stat(filepath.Join(string(d), "vmm.lock")); err != nil {
		t.Error(err)
	}
}
