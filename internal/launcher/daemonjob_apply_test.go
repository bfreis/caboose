package launcher

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/daemonproto"
	"github.com/bfreis/caboose/internal/proposal"
)

// buildWorld is proposalsWorld's alpha, its apko image built by a fake
// that says more lines than a job keeps (escapes and tabs among them), and
// leaves writing its lock to whoever applies, as an apko build under
// apply does.
type buildWorld struct {
	*proposalsWorld

	mu       sync.Mutex
	err      error         // what the build fails with
	hold     chan struct{} // when set, the build waits for it to close
	building chan struct{} // closed once a build has started
	during   func()        // when set, runs in the build
	built    [][]string    // the packages of each build
	configs  []string      // config.toml as each build found it
	locks    []string      // config.toml as each lock was written
	notes    []string      // the notifications
	clock    time.Time
}

func newBuildWorld(t *testing.T) *buildWorld {
	t.Helper()
	pw := newProposalsWorld(t, "")
	w := &buildWorld{proposalsWorld: pw, building: make(chan struct{}), clock: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)}
	pw.l = NewDaemonLinks(DaemonLinksOptions{Dir: filepath.Join(pw.home, "daemon-build"), CabooseHome: pw.home,
		NewApp: func(env string) (*App, error) {
			app, err := pw.newApp(env)
			if err != nil {
				return nil, err
			}
			app.buildImage = func() error { return w.build(app) }
			return app, nil
		},
		JobNotify: func(string) func(title, text string) {
			return func(title, text string) {
				w.mu.Lock()
				w.notes = append(w.notes, title+"|"+text)
				w.mu.Unlock()
			}
		}})
	pw.l.jobNow = func() time.Time {
		w.mu.Lock()
		defer w.mu.Unlock()
		return w.clock
	}
	t.Cleanup(pw.l.Close)
	return w
}

func (w *buildWorld) build(app *App) error {
	w.mu.Lock()
	w.built = append(w.built, slices.Clone(app.Cfg.ImageProfile.Packages))
	w.configs = append(w.configs, w.readConfig())
	err, hold, during := w.err, w.hold, w.during
	select {
	case <-w.building:
	default:
		close(w.building)
	}
	w.mu.Unlock()
	for i := range 30 {
		fmt.Fprintf(app.Stderr, "\x1b[34m#%d\x1b[0m step\t%d of 30\r\n", i, i)
	}
	fmt.Fprint(app.Stderr, strings.Repeat("long ", 100)+"\n\n")
	if hold != nil {
		<-hold
	}
	if during != nil {
		during()
	}
	if err != nil {
		return err
	}
	if !app.deferLock {
		w.t.Error("the job's build would write the lock itself")
	}
	app.pendingLock = func() error {
		w.mu.Lock()
		defer w.mu.Unlock()
		w.locks = append(w.locks, w.readConfig())
		return nil
	}
	return nil
}

func (w *buildWorld) start(file, hash, typed string) (*daemonproto.JobInfo, error) {
	w.t.Helper()
	p := daemonproto.ProposalApplyParams{Env: "alpha", File: file, Hash: hash, Typed: typed}
	if err := p.Validate(); err != nil {
		w.t.Fatal(err)
	}
	j, err := w.l.ProposalBuild(w.t.Context(), p)
	if err == nil {
		if verr := (&daemonproto.JobResult{Job: *j}).Validate(); verr != nil {
			w.t.Fatalf("the daemon's answer does not validate: %v", verr)
		}
	}
	return j, err
}

// wait waits for alpha's latest job to end, and returns it as status
// shows it, checked as the protocol checks it.
func (w *buildWorld) wait() daemonproto.JobInfo {
	w.t.Helper()
	e := w.l.env("alpha")
	e.mu.Lock()
	j := e.job
	e.mu.Unlock()
	if j == nil {
		w.t.Fatal("no job")
	}
	select {
	case <-j.done:
	case <-time.After(30 * time.Second):
		w.t.Fatal("the job never ended")
	}
	return w.shown()
}

// shown is alpha's job as status shows it.
func (w *buildWorld) shown() daemonproto.JobInfo {
	w.t.Helper()
	for _, st := range w.l.Envs() {
		if st.Name == "alpha" && st.Job != nil {
			if err := (&daemonproto.StatusResult{Envs: []daemonproto.EnvStatus{st}}).Validate(); err != nil {
				w.t.Fatalf("status: %v", err)
			}
			return *st.Job
		}
	}
	w.t.Fatalf("no job in status: %+v", w.l.Envs())
	return daemonproto.JobInfo{}
}

// noJob fails when alpha has had a job.
func (w *buildWorld) noJob() {
	w.t.Helper()
	e := w.l.env("alpha")
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.job != nil {
		w.t.Errorf("a job started: %+v", e.job.info)
	}
}

func (w *buildWorld) notified() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.notes)
}

const dotProposal = "title = \"Add graphviz\"\n[packages]\nadd = [\"graphviz\"]\n"

// A [packages] proposal is listed for the page, applied by proposal_build
// alone: a job answered at once, which builds the image with the packages
// before anything is written, then writes them into config.toml, the lock
// after it, and removes the proposal. Status shows its last lines as it
// goes, each one printable and short, then its notes, the restart among
// them.
func TestDaemonBuildJob(t *testing.T) {
	w := newBuildWorld(t)
	p := w.propose("dot.toml", dotProposal)
	ps := w.list()
	if len(ps) != 1 || !ps[0].WebApply || !ps[0].NeedsBuild || ps[0].WebApplyWhy != "" {
		t.Fatalf("%+v", ps)
	}
	v := ps[0]
	if _, err := w.apply("dot.toml", v.Hash, ""); daemonproto.Kind(err) != daemonproto.KindBadRequest || !strings.Contains(err.Error(), daemonproto.MethodProposalBuild) {
		t.Errorf("proposal_apply: %v", err)
	}
	if strings.Contains(w.readConfig(), "graphviz") {
		t.Fatalf("written by proposal_apply:\n%s", w.readConfig())
	}
	w.mu.Lock()
	w.hold = make(chan struct{})
	w.mu.Unlock()
	j, err := w.start("dot.toml", v.Hash, "")
	if err != nil {
		t.Fatal(err)
	}
	if j.Kind != daemonproto.JobApply || j.State != daemonproto.JobRunning || j.Proposal != "dot.toml" {
		t.Errorf("%+v", j)
	}
	<-w.building
	// The build's lines reach status while it runs.
	deadline := time.Now().Add(10 * time.Second)
	for {
		s := w.shown()
		if s.State != daemonproto.JobRunning {
			t.Fatalf("ended while held: %+v", s)
		}
		if len(s.Lines) == daemonproto.MaxJobLines {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("lines %q", s.Lines)
		}
		time.Sleep(10 * time.Millisecond)
	}
	close(w.hold)
	got := w.wait()
	if got.State != daemonproto.JobDone || got.Error != "" {
		t.Fatalf("%+v\n%s", got, w.jobsLogOf())
	}
	if len(got.Lines) > daemonproto.MaxJobLines || !slices.Contains(got.Lines, "Built image 'caboose:alpha'.") {
		t.Errorf("lines %q", got.Lines)
	}
	for _, l := range got.Lines {
		if len(l) > daemonproto.MaxJobLine || strings.ContainsAny(l, "\x1b\t\r") || strings.Contains(l, `\u001B`) {
			t.Errorf("line %q", l)
		}
	}
	n := strings.Join(got.Notes, "\n")
	for _, want := range []string{"Built the image with the packages of [apko.default]", "keeps its current image until its next restart", "caboose -e alpha restart"} {
		if !strings.Contains(n, want) {
			t.Errorf("no %q in the notes %q", want, got.Notes)
		}
	}
	if len(w.built) != 1 || !slices.Equal(w.built[0], []string{"graphviz"}) || strings.Contains(w.configs[0], "graphviz") {
		t.Errorf("built %q on\n%s", w.built, w.configs)
	}
	f, err := config.ParseFile("config.toml", []byte(w.readConfig()))
	if err != nil {
		t.Fatal(err)
	}
	if prof, err := f.ApkoProfile("default"); err != nil || !slices.Equal(prof.Packages, []string{"graphviz"}) {
		t.Errorf("profile %+v %v\n%s", prof, err, w.readConfig())
	}
	if len(w.locks) != 1 || !strings.Contains(w.locks[0], "graphviz") {
		t.Errorf("the lock, written on %q", w.locks)
	}
	gone(t, p)
	if nt := w.notified(); len(nt) != 1 || !strings.Contains(nt[0], "applied the proposal dot.toml") || !strings.Contains(nt[0], "caboose -e alpha restart") {
		t.Errorf("notified %q", nt)
	}
	if log := w.jobsLogOf(); !strings.Contains(log, "apply alpha: done") || !strings.Contains(log, "step") {
		t.Errorf("jobs.log:\n%s", log)
	}
}

func (w *buildWorld) jobsLogOf() string {
	b, _ := os.ReadFile(filepath.Join(config.EnvDirFor(w.home, "alpha"), jobsLogFile))
	return string(b)
}

// A list that does not build leaves config.toml and the proposal as they
// were, writes no lock, and the job failed says nothing was written.
func TestDaemonBuildJobFails(t *testing.T) {
	w := newBuildWorld(t)
	p := w.propose("dot.toml", dotProposal)
	v := w.list()[0]
	before, _ := os.ReadFile(p)
	cfg := w.readConfig()
	w.err = errors.New("graphviz: no such package")
	if _, err := w.start("dot.toml", v.Hash, ""); err != nil {
		t.Fatal(err)
	}
	got := w.wait()
	if got.State != daemonproto.JobFailed || !strings.Contains(got.Error, "no such package") || !strings.Contains(got.Error, "nothing was written") || len(got.Notes) != 0 {
		t.Errorf("%+v", got)
	}
	if w.readConfig() != cfg {
		t.Errorf("config.toml changed:\n%s", w.readConfig())
	}
	if after, _ := os.ReadFile(p); string(after) != string(before) {
		t.Errorf("the proposal changed: %q", after)
	}
	if len(w.locks) != 0 {
		t.Errorf("a lock written: %q", w.locks)
	}
	if nt := w.notified(); len(nt) != 1 || !strings.Contains(nt[0], "applying the proposal dot.toml to alpha failed: the build failed") {
		t.Errorf("notified %q", nt)
	}
}

// One job at a time: while the build runs, another proposal_build and a
// restart are busy, and a stale hash or a wrong name is answered at once,
// with no job started.
func TestDaemonBuildJobRefusals(t *testing.T) {
	w := newBuildWorld(t)
	w.propose("dot.toml", dotProposal)
	w.propose("mixed.toml", "title = \"Both\"\n[packages]\nadd = [\"graphviz\"]\n[roots]\nother = \"~/src/other\"\n")
	w.propose("other.toml", otherRoot)
	byFile := map[string]daemonproto.ProposalInfo{}
	for _, p := range w.list() {
		byFile[p.File] = p
	}
	if _, err := w.start("mixed.toml", byFile["mixed.toml"].Hash, "othre"); daemonproto.Kind(err) != daemonproto.KindBadRequest || !strings.Contains(err.Error(), "not other") {
		t.Errorf("a wrong name: %v", err)
	}
	if _, err := w.start("other.toml", byFile["other.toml"].Hash, "other"); daemonproto.Kind(err) != daemonproto.KindBadRequest || !strings.Contains(err.Error(), daemonproto.MethodProposalApply) {
		t.Errorf("one that builds nothing: %v", err)
	}
	if _, err := w.start("dot.toml", strings.Repeat("0", 64), ""); daemonproto.Kind(err) != daemonproto.KindStaleDiff {
		t.Errorf("another hash: %v", err)
	}
	if _, err := w.start("nope.toml", byFile["dot.toml"].Hash, ""); daemonproto.Kind(err) != daemonproto.KindNotFound {
		t.Errorf("no such proposal: %v", err)
	}
	w.propose("dot.toml", strings.Replace(dotProposal, "Add graphviz", "Add graphviz now", 1))
	if _, err := w.start("dot.toml", byFile["dot.toml"].Hash, ""); daemonproto.Kind(err) != daemonproto.KindStaleDiff {
		t.Errorf("the proposal changed since it was shown: %v", err)
	}
	w.config("[session]\nauto_sync = true\n")
	if _, err := w.start("mixed.toml", byFile["mixed.toml"].Hash, "other"); daemonproto.Kind(err) != daemonproto.KindStaleDiff {
		t.Errorf("config.toml changed since it was shown: %v", err)
	}
	w.noJob()
	if len(w.built) != 0 {
		t.Errorf("built %q", w.built)
	}

	w.config("")
	w.propose("dot.toml", dotProposal)
	var v daemonproto.ProposalInfo
	for _, p := range w.list() {
		if p.File == "dot.toml" {
			v = p
		}
	}
	w.hold = make(chan struct{})
	if _, err := w.start(v.File, v.Hash, ""); err != nil {
		t.Fatal(err)
	}
	<-w.building
	if _, err := w.start(v.File, v.Hash, ""); daemonproto.Kind(err) != daemonproto.KindBusy {
		t.Errorf("a second build: %v", err)
	}
	close(w.hold)
	if got := w.wait(); got.State != daemonproto.JobDone {
		t.Errorf("%+v", got)
	}
}

// What changed while the image was built is not written over: the
// profile's packages edited in config.toml, or the proposal itself; the
// job fails, and config.toml is as the edit left it, the proposal pending.
func TestDaemonBuildJobChangedMeanwhile(t *testing.T) {
	for _, c := range []struct {
		name   string
		during func(w *buildWorld)
		want   string
	}{
		{"profile", func(w *buildWorld) { w.config("[apko.default]\npackages = [\"jq\"]\n") }, "changed while the image was built"},
		{"proposal", func(w *buildWorld) {
			w.propose("dot.toml", strings.Replace(dotProposal, "Add graphviz", "Add graphviz now", 1))
		}, "changed while the image was built"},
		{"deleted", func(w *buildWorld) { os.Remove(filepath.Join(w.dataDir("alpha"), proposal.Dir, "dot.toml")) }, "deleted"},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := newBuildWorld(t)
			w.propose("dot.toml", dotProposal)
			v := w.list()[0]
			w.during = func() { c.during(w) }
			if _, err := w.start("dot.toml", v.Hash, ""); err != nil {
				t.Fatal(err)
			}
			got := w.wait()
			if got.State != daemonproto.JobFailed || !strings.Contains(got.Error, c.want) || !strings.Contains(got.Error, "nothing was written") {
				t.Errorf("%+v", got)
			}
			if strings.Contains(w.readConfig(), "graphviz") || len(w.locks) != 0 {
				t.Errorf("written:\n%s\nlocks %q", w.readConfig(), w.locks)
			}
		})
	}
}

// Packages with a root are one config.toml edit, with the root's name
// typed: both written once the image is built.
func TestDaemonBuildJobWithRoot(t *testing.T) {
	w := newBuildWorld(t)
	p := w.propose("mixed.toml", "title = \"Both\"\n[packages]\nadd = [\"graphviz\"]\n[roots]\nother = \"~/src/other\"\n")
	v := w.list()[0]
	if !v.WebApply || !v.NeedsBuild || v.Confirm.Kind != daemonproto.ProposalConfirmTypeName || v.Confirm.Name != "other" || len(v.Parts) != 2 {
		t.Fatalf("%+v", v)
	}
	if _, err := w.start("mixed.toml", v.Hash, "other"); err != nil {
		t.Fatal(err)
	}
	got := w.wait()
	if got.State != daemonproto.JobDone {
		t.Fatalf("%+v", got)
	}
	if n := strings.Join(got.Notes, "\n"); !strings.Contains(n, "mounts") || !strings.Contains(n, "/work/other") || !strings.Contains(n, "keeps its current image") {
		t.Errorf("notes %q", got.Notes)
	}
	f, err := config.ParseFile("config.toml", []byte(w.readConfig()))
	if err != nil {
		t.Fatal(err)
	}
	prof, _ := f.ApkoProfile("default")
	if f.Roots["other"].Host != "~/src/other" || f.Roots["dev"].Host != "~/dev" || !slices.Equal(prof.Packages, []string{"graphviz"}) {
		t.Errorf("roots %+v, packages %q\n%s", f.Roots, prof.Packages, w.readConfig())
	}
	if len(w.locks) != 1 || !strings.Contains(w.locks[0], "other") {
		t.Errorf("locks %q", w.locks)
	}
	gone(t, p)
}
