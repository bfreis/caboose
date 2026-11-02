package e2e

import (
	"bufio"
	"bytes"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/agentproto"
	"github.com/bfreis/caboose/internal/backend"
	"github.com/bfreis/caboose/internal/hostlink"
	"github.com/bfreis/caboose/internal/vm"
	"github.com/bfreis/caboose/internal/vm/vnodes"
)

// heldWatch is a watcher in the guest: it watches each directory named
// after the seconds to run for IN_ATTRIB, says "ready" once it does,
// then prints DIR/NAME for each event.
const heldWatch = `import ctypes,os,select,struct,sys,time
libc=ctypes.CDLL(None,use_errno=True)
fd=libc.inotify_init1(os.O_NONBLOCK)
wds={}
for d in sys.argv[2:]:
    wds[libc.inotify_add_watch(fd,d.encode(),0x4)]=d
print("ready",flush=True)
end=time.time()+float(sys.argv[1])
while time.time()<end:
    r,_,_=select.select([fd],[],[],0.2)
    if not r: continue
    b=os.read(fd,65536); i=0
    while i+16<=len(b):
        wd,mask,cookie,n=struct.unpack_from("iIII",b,i)
        print(wds.get(wd,"?")+"/"+b[i+16:i+16+n].rstrip(b"\0").decode(),flush=True)
        i+=16+n
`

// lines collects what a guest command prints, as it prints it, and when
// each line first came.
type lines struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	first map[string]time.Time
	part  string
}

func (l *lines) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if l.first == nil {
		l.first = map[string]time.Time{}
	}
	l.part += string(p)
	for {
		i := strings.IndexByte(l.part, '\n')
		if i < 0 {
			break
		}
		if s := l.part[:i]; l.first[s].IsZero() {
			l.first[s] = now
		}
		l.part = l.part[i+1:]
	}
	return l.buf.Write(p)
}

// seenAt is when the line s first came.
func (l *lines) seenAt(s string) time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.first[s]
}

// seen is how many distinct names in dir the watcher has printed.
func (l *lines) seen(dir string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	names := map[string]bool{}
	for _, s := range strings.Split(l.buf.String(), "\n") {
		if n, ok := strings.CutPrefix(s, dir+"/"); ok && n != "" {
			names[n] = true
		}
	}
	return len(names)
}

func (l *lines) has(s string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Contains(l.buf.String(), s)
}

// TestHeldRelay checks what the host link's file-change relay costs the
// Mac under vm. It boots a VM with a share at /work, links to its agent
// as the launcher does, with the share relayed, and for each case writes
// files on the Mac, waits for a watcher in the guest to see every one,
// and deletes them: "fast" at once (as a tool's cookie files are),
// "slow" after a few seconds. It fails when the watcher misses a file,
// or when the VM's process still holds a deleted one open once the guest
// has forgotten other files after them. Runs only when CABOOSE_VM_E2E_HELD is set; _FILES is how many
// files each case writes (40: past the relay's 64 for a directory, it
// would send the directory instead). The "bulk" case writes 200, which
// the guest looks up itself, and deletes them at once: the relay sends
// their directory, and the gone paths apart. With
// CABOOSE_VM_E2E_HELD_NORECHECK set, the guest's environment does not say
// vm, and the agent does none of its lookups: the cases should fail. At
// the end it times the lookups a recheck does, and runs bursts of
// changes, then counts the change messages the agent dropped (its
// console says).
func TestHeldRelay(t *testing.T) {
	dir := os.Getenv("CABOOSE_VM_E2E")
	if dir == "" || os.Getenv("CABOOSE_VM_E2E_HELD") == "" {
		t.Skip("CABOOSE_VM_E2E_HELD is not set")
	}
	n := 40
	if s := os.Getenv("CABOOSE_VM_E2E_HELD_FILES"); s != "" {
		var err error
		if n, err = strconv.Atoi(s); err != nil || n <= 0 {
			t.Fatalf("CABOOSE_VM_E2E_HELD_FILES=%q", s)
		}
	}
	// In DIR, so that nothing is written elsewhere, and short enough for
	// vm.sock's path to fit a Unix socket's.
	work, err := os.MkdirTemp(dir, "w")
	if err != nil {
		t.Fatal(err)
	}
	if work, err = filepath.EvalSymlinks(work); err != nil {
		t.Fatal(err)
	}
	share := filepath.Join(work, "share")
	cases := []string{"fast", "slow", "bulk"}
	for _, c := range cases {
		if err := os.MkdirAll(filepath.Join(share, c), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	vmdir := vm.Dir(filepath.Join(work, "vm"))
	v := backend.NewVM("e2e", vmdir, host{dir: dir, work: work})
	v.Self, _ = os.Executable()
	v.Ready = ready
	defer func() {
		for _, f := range []string{vmdir.Log(), vmdir.Console()} {
			if b, err := os.ReadFile(f); err == nil {
				base := strings.TrimSuffix(filepath.Base(f), ".log")
				_ = os.WriteFile(filepath.Join(dir, base+"-held.log"), b, 0o644)
			}
		}
		_ = v.Remove()
		os.RemoveAll(work)
	}()
	start := time.Now()
	spec := backend.Spec{
		Image: "e2e", Hostname: "caboose-e2e", User: "0:0",
		Mounts: []backend.Mount{{Source: share, Target: "/work"}},
		Labels: []string{"caboose.isolation=vm"},
	}
	recheck := os.Getenv("CABOOSE_VM_E2E_HELD_NORECHECK") == ""
	if recheck {
		// As the launcher's spec under vm: the link reads it.
		spec.Env = []string{"CABOOSE_ISOLATION=vm"}
	}
	step(t, start, "the agent's rechecks: %v", recheck)
	if err := v.Create(spec); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := v.WaitReady(90 * time.Second); err != nil {
		t.Fatalf("ready: %v", err)
	}
	step(t, start, "ready")

	// The link, with the share relayed, as the launcher's under vm.
	c, err := v.DialLink()
	if err != nil {
		t.Fatalf("link: %v", err)
	}
	sess := agentproto.NewSession(c, c, true)
	linked := make(chan error, 1)
	go func() {
		linked <- hostlink.Run(sess, hostlink.Config{
			Relay: []hostlink.Root{{Host: share, Container: "/work"}},
			Log:   log.New(testWriter{t}, "link: ", 0),
		})
	}()
	defer func() {
		sess.Close()
		<-linked
	}()

	sys, err := vnodes.Host()
	if err != nil {
		t.Fatal(err)
	}
	f := &vnodes.Finder{Sys: sys, Dirs: []string{work}}
	s, err := f.Sample()
	if err != nil {
		t.Fatalf("the VM's process: %v", err)
	}
	pid := s.PID
	// held is how many files under d the VM's process has open, and how
	// many of those have no name left.
	held := func(d string) (open, deleted int) {
		t.Helper()
		fds, err := sys.VnodeFDs(pid)
		if err != nil {
			t.Fatal(err)
		}
		for _, fd := range fds {
			fi, err := sys.FDFile(pid, fd)
			if err != nil || !fi.Regular || !strings.HasPrefix(fi.Path, d+"/") {
				continue
			}
			open++
			if fi.Links == 0 {
				deleted++
			}
		}
		return open, deleted
	}
	step(t, start, "VM process %d, %d files open", pid, s.Files)

	var out lines
	args := []string{"python3", "-c", heldWatch, "120"}
	for _, c := range cases {
		args = append(args, "/work/"+c)
	}
	watcher := v.Command(backend.ExecSpec{Argv: args})
	watcher.Stdout, watcher.Stderr = &out, &out
	if err := watcher.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = watcher.Process.Kill(); _ = watcher.Wait() }()
	waitFor := func(what string, d time.Duration, ok func() bool) bool {
		t.Helper()
		for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
			if ok() {
				return true
			}
		}
		t.Errorf("%s: not after %v", what, d)
		return false
	}
	if !waitFor("the watcher", 10*time.Second, func() bool { return out.has("ready") }) {
		t.Fatalf("watcher said: %s", out.buf.String())
	}

	data := make([]byte, 1<<20)
	for i := range data {
		data[i] = byte(i)
	}
	files := map[string]int{"fast": n, "slow": n, "bulk": 200}
	for _, c := range cases {
		d := filepath.Join(share, c)
		k := files[c]
		t0 := time.Now()
		body := data
		if c == "bulk" {
			// Empty, so that they come in one of the relay's batches.
			body = nil
		}
		for i := range k {
			if err := os.WriteFile(filepath.Join(d, fmt.Sprintf("f%03d", i)), body, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if c == "bulk" {
			// Past the relay's 64 it sends the directory, not the
			// files: the guest looks them up itself, as a tool listing
			// it would, and lets their entries time out.
			time.Sleep(time.Second)
			guest(v, `for f in /work/bulk/f*; do stat "$f" >/dev/null 2>&1; done; true`)
			time.Sleep(3 * time.Second)
			open, _ := held(d)
			step(t, start, "%s: the guest looked %d files up, the VM's process has %d open", c, k, open)
		} else {
			all := waitFor(c+": the watcher seeing every file", 10*time.Second, func() bool { return out.seen("/work/"+c) >= k })
			open, _ := held(d)
			step(t, start, "%s: the watcher saw %d of %d files in %.2fs; the VM's process has %d open", c, out.seen("/work/"+c), k, time.Since(t0).Seconds(), open)
			if !all {
				continue
			}
		}
		if c == "slow" {
			time.Sleep(4 * time.Second)
			open, _ := held(d)
			step(t, start, "%s: 4 s later, %d open", c, open)
		}
		// How long after the watcher saw each file (its touch) the Mac
		// deletes it: the fast case is about files deleted within the
		// guest's second of trusting its entry.
		within, least, most := 0, time.Duration(1<<62), time.Duration(0)
		for i := range k {
			name := fmt.Sprintf("f%03d", i)
			at := time.Now()
			if err := os.Remove(filepath.Join(d, name)); err != nil {
				t.Fatal(err)
			}
			if seen := out.seenAt("/work/" + c + "/" + name); !seen.IsZero() {
				gap := at.Sub(seen)
				least, most = min(least, gap), max(most, gap)
				if gap < time.Second {
					within++
				}
			}
		}
		if c != "bulk" {
			step(t, start, "%s: deleted %v to %v after the watcher saw each; %d of %d within 1 s", c, least.Round(time.Millisecond), most.Round(time.Millisecond), within, k)
		}
		if c == "fast" && within < k {
			t.Errorf("fast: only %d of %d files deleted within 1 s of their touch: the case does not test what it is for", within, k)
		}
		time.Sleep(5 * time.Second)
		open, deleted := held(d)
		step(t, start, "%s: 5 s after the Mac deleted them, %d open, %d of them deleted; %s", c, open, deleted, lsofDeleted(pid, d))
	}
	step(t, start, "guest: dentry-state, inode-nr: %s", guest(v, "cat /proc/sys/fs/dentry-state /proc/sys/fs/inode-nr | tr '\\n\\t' '  '"))

	// The last forgets of a burst can wait for later ones before the
	// Mac's side lets go of their files: some, for files that are none of
	// the cases' (looked up, deleted on the Mac, looked up again past the
	// entry's timeout), after which what is still open was never
	// forgotten.
	nudge := filepath.Join(share, "nudge")
	if err := os.MkdirAll(nudge, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := range 200 {
		if err := os.WriteFile(filepath.Join(nudge, fmt.Sprintf("n%03d", i)), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	const statAll = `for f in /work/nudge/n*; do stat "$f" >/dev/null 2>&1; done; true`
	guest(v, statAll)
	if err := os.RemoveAll(nudge); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1500 * time.Millisecond)
	guest(v, `for i in $(seq -w 0 199); do stat /work/nudge/n$i >/dev/null 2>&1; done; true`)
	time.Sleep(2 * time.Second)
	for _, c := range cases {
		d := filepath.Join(share, c)
		open, deleted := held(d)
		step(t, start, "%s: after other files' forgets, %d open, %d of them deleted; %s", c, open, deleted, lsofDeleted(pid, d))
		if deleted > 0 {
			t.Errorf("%s: the VM's process holds %d of %d deleted files open", c, deleted, files[c])
		}
	}
	if d, err := f.Deleted(time.Now().Add(10 * time.Second)); err == nil {
		step(t, start, "Finder.Deleted: %+v", d)
	}

	// What a recheck of recheckMax paths costs: lstat of 4,096 files the
	// guest looked up, past its entries' second, there and then gone.
	timing := filepath.Join(share, "timing")
	for i := range 64 {
		sub := filepath.Join(timing, fmt.Sprintf("d%02d", i))
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		for j := range 64 {
			if err := os.WriteFile(filepath.Join(sub, fmt.Sprintf("f%02d", j)), nil, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	const lstatAll = `import os,sys,time
ps=[os.path.join(r,f) for r,_,fs in os.walk("/work/timing") for f in fs] if sys.argv[1]=="walk" else ["/work/timing/d%02d/f%02d"%(i,j) for i in range(64) for j in range(64)]
t=time.perf_counter(); k=0
for p in ps:
    try: os.lstat(p); k+=1
    except OSError: pass
print("%d paths, %d found, %.3f s"%(len(ps),k,time.perf_counter()-t))`
	step(t, start, "timing: first lookup (walk): %s", guest(v, "python3 -c '"+lstatAll+"' walk"))
	for round := range 3 {
		time.Sleep(2500 * time.Millisecond)
		step(t, start, "timing: lstat again past the entries' second (%d): %s", round+1, guest(v, "python3 -c '"+lstatAll+"' list"))
	}
	if err := os.RemoveAll(timing); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2500 * time.Millisecond)
	step(t, start, "timing: lstat of them gone: %s", guest(v, "python3 -c '"+lstatAll+"' list"))

	// Bursts as a branch switch makes: 3,600 files in 60 directories (a
	// batch the relay sends path by path), written then deleted, every
	// half second, the rechecks of each coming due during the next ones.
	burst := filepath.Join(share, "burst")
	tb := time.Now()
	for round := range 10 {
		for i := range 60 {
			sub := filepath.Join(burst, fmt.Sprintf("r%d/d%02d", round, i))
			if err := os.MkdirAll(sub, 0o755); err != nil {
				t.Fatal(err)
			}
			for j := range 60 {
				if err := os.WriteFile(filepath.Join(sub, fmt.Sprintf("f%02d", j)), nil, 0o644); err != nil {
					t.Fatal(err)
				}
			}
		}
		time.Sleep(500 * time.Millisecond)
		if err := os.RemoveAll(filepath.Join(burst, fmt.Sprintf("r%d", round))); err != nil {
			t.Fatal(err)
		}
	}
	step(t, start, "bursts: 10 of 3,600 files written and deleted in %.1fs", time.Since(tb).Seconds())
	time.Sleep(5 * time.Second)
	console, _ := os.ReadFile(vmdir.Console())
	drops := 0
	for _, s := range strings.Split(string(console), "\n") {
		if strings.Contains(s, "change messages dropped") {
			drops++
			step(t, start, "console: %s", s)
		}
	}
	step(t, start, "bursts: %d drop lines on the console", drops)
	if open, deleted := held(burst); deleted > 0 {
		step(t, start, "bursts: %d open, %d of them deleted, 5 s after", open, deleted)
	}
}

// guest runs script in the guest, and says what it printed.
func guest(v *backend.VM, script string) string {
	s, err := backend.Output(v, "sh", "-c", script)
	if err != nil {
		return fmt.Sprintf("%v: %s", err, s)
	}
	return s
}

// lsofDeleted is what lsof says pid holds under d of deleted files, for a
// second opinion.
func lsofDeleted(pid int, d string) string {
	out, err := exec.Command("/usr/sbin/lsof", "-nP", "-a", "+L1", "-p", strconv.Itoa(pid), "-F", "n").Output()
	if err != nil && len(out) == 0 {
		return fmt.Sprintf("lsof: %v", err)
	}
	k := 0
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "n"+d+"/") {
			k++
		}
	}
	return fmt.Sprintf("lsof +L1: %d", k)
}

// testWriter logs what is written to it.
type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}
