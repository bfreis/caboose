package launcher

import (
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/vm/vnodes"
)

// fakeVnodes answers each Sample with the next of its counts, out of 1000,
// or the next error.
type fakeVnodes struct {
	files []int
	errs  []error
}

func (f *fakeVnodes) Sample() (vnodes.Sample, error) {
	n, err := f.files[0], f.errs[0]
	f.files, f.errs = f.files[1:], f.errs[1:]
	if err != nil {
		return vnodes.Sample{}, err
	}
	return vnodes.Sample{PID: 42, Files: n, Max: 1000}, nil
}

// runVnodeWatch checks once a minute for each count (-1 for errs' next),
// and returns the notifications' texts and the log lines, in order.
func runVnodeWatch(counts []int, errs ...error) (notes, logs []string) {
	f := &fakeVnodes{}
	for _, c := range counts {
		var err error
		if c < 0 {
			err, errs = errs[0], errs[1:]
		}
		f.files, f.errs = append(f.files, c), append(f.errs, err)
	}
	w := &vnodeWatch{sampler: f, env: "default"}
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	for range counts {
		text, notify, l := w.check(now)
		if notify {
			notes = append(notes, text)
		}
		logs = append(logs, l...)
		now = now.Add(vnodePollEvery)
	}
	return notes, logs
}

func TestVnodeWatchTellsEachLevelOnce(t *testing.T) {
	notes, logs := runVnodeWatch([]int{100, 499, 500, 700, 790, 800, 950, 810, 600, 820, 410, 900})
	want := []string{
		"the VM holds 500 of the Mac's 1,000 vnodes: see caboose doctor",
		"the VM holds 800 of the Mac's 1,000 vnodes: see caboose doctor",
	}
	if strings.Join(notes, "\n") != strings.Join(want, "\n") {
		t.Fatalf("notified %q, want %q", notes, want)
	}
	if len(logs) != 2 || !strings.Contains(logs[0], "past 50%") || !strings.Contains(logs[1], "past 80%") {
		t.Fatalf("logged %q, want the two crossings", logs)
	}
}

func TestVnodeWatchJumpsStraightToTheAlarm(t *testing.T) {
	notes, _ := runVnodeWatch([]int{850, 600, 900})
	if len(notes) != 1 || !strings.Contains(notes[0], "850") {
		t.Fatalf("notified %q, want the 80%% level once", notes)
	}
}

func TestVnodeWatchRearmsUnder40(t *testing.T) {
	// Falling to 400 is not under 40%: nothing is told again until 399.
	notes, logs := runVnodeWatch([]int{550, 400, 560, 399, 550, 850, 399, 820})
	if len(notes) != 4 {
		t.Fatalf("notified %q, want 550, 550 again after 399, 850, and 820 after 399", notes)
	}
	for i, n := range []string{"550", "550", "850", "820"} {
		if !strings.Contains(notes[i], " "+n+" ") {
			t.Errorf("notification %d is %q, want it to say %s", i, notes[i], n)
		}
	}
	under := 0
	for _, l := range logs {
		if strings.Contains(l, "under 40%") {
			under++
		}
	}
	if under != 2 {
		t.Errorf("logged %q, want two falls under 40%%", logs)
	}
}

func TestVnodeWatchLogsAFailureHourly(t *testing.T) {
	gone := errors.New("no VM is running")
	other := errors.New("none of the 1 running VMs holds a file of this environment's")
	counts := make([]int, 0, 130)
	var errs []error
	for range 61 {
		counts, errs = append(counts, -1), append(errs, gone)
	}
	counts, errs = append(counts, -1), append(errs, other)
	// The VM back: crossings still told, failures between them aside.
	counts = append(counts, 600, -1, 700)
	errs = append(errs, gone)
	notes, logs := runVnodeWatch(counts, errs...)
	var failures []string
	for _, l := range logs {
		if strings.HasPrefix(l, "cannot count") {
			failures = append(failures, l)
		}
	}
	// gone at 0 and again at 60 minutes, other once; gone at 63 minutes is
	// within the hour of the one at 60.
	if len(failures) != 3 || !strings.Contains(failures[2], "none of the 1") {
		t.Fatalf("logged %q, want gone twice and other once", failures)
	}
	if len(notes) != 1 {
		t.Fatalf("notified %q, want 600 once", notes)
	}
}

func TestVnodeWatchNoNotificationsWithoutAVM(t *testing.T) {
	notes, _ := runVnodeWatch([]int{-1, -1}, vnodes.ErrUnsupported, errors.New("x"))
	if len(notes) != 0 {
		t.Fatalf("notified %q", notes)
	}
}

func TestThousands(t *testing.T) {
	for n, want := range map[int]string{0: "0", 999: "999", 1000: "1,000", 263168: "263,168", 1234567: "1,234,567", -4500: "-4,500"} {
		if got := thousands(n); got != want {
			t.Errorf("thousands(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestVnodeTitle(t *testing.T) {
	if got := vnodeTitle("work"); got != "caboose (work)" {
		t.Fatalf("got %q", got)
	}
}

func TestVMFilesHeldOffAMac(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("a Mac has Virtualization processes to read")
	}
	a := &App{Cfg: &config.Config{DataDir: t.TempDir()}}
	if _, _, _, err := a.vmFilesHeld(); !errors.Is(err, vnodes.ErrUnsupported) {
		t.Fatalf("got %v, want %v", err, vnodes.ErrUnsupported)
	}
}
