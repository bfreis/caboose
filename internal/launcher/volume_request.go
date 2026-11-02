package launcher

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"regexp"
	"strings"

	"github.com/bfreis/caboose/internal/backend"
	"github.com/bfreis/caboose/internal/broker"
	"github.com/bfreis/caboose/internal/cappedlog"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/daemonproto"
	"github.com/bfreis/caboose/internal/hostlink"
	"github.com/bfreis/caboose/internal/policy"
)

// A volume resize the sandbox asks for (caboose-agent request
// volume-resize NAME SIZE). The link resolves what the host knows of the
// volume (linkRunner.VolumeFacts), the broker decides and files it, and a
// person allows it once (caboose requests, the web page, the menu-bar
// app). The allowed request starts a job of the environment's
// (volumeJobs), which makes the resize caboose volume resize makes while
// the VM runs -- grow, or live shrink -- and writes the new size to
// config.toml once the disk has it; what came of it goes to jobs.log, the
// daemon's log, a notification and, as the request's outcome, back to the
// sandbox. Only a link the daemon runs offers it: the job is the daemon's.

// VolumeFacts is hostlink.VolumeFacts's, from this environment's VM as it
// is now: its disk's size, whether the running VM has the volume on one of
// caboose-vmm's disks, and the space free on the data dir's volume (-1
// when that cannot be read).
func (r *linkRunner) VolumeFacts(name string) (size int64, live bool, free int64, err error) {
	a := r.a
	vs, err := a.vmVolumeState(name)
	if err != nil {
		return 0, false, 0, err
	}
	free, ferr := vmFreeSpace(a.Cfg.DataDir)
	if ferr != nil {
		free = -1
	}
	return a.volumeDiskSize(name, 0), vs.Running && vs.Has && vs.Live, free, nil
}

var _ hostlink.VolumeFacts = (*linkRunner)(nil)

// volumeJobs starts the resizes a person allowed for environment env, each
// as a job of the environment's.
type volumeJobs struct {
	l   *DaemonLinks
	env string
}

var _ broker.Resizer = volumeJobs{}

// ResizeVolume is broker.Resizer's: the job started, or why not (another
// job runs for the environment, the daemon stops or is being replaced).
// The request's outcome, which the sandbox reads, has the error with the
// host's paths left out (notStarted); the answer and the daemon's log keep
// the whole of it, for the person.
func (v volumeJobs) ResizeVolume(r broker.Resize) error {
	l := v.l
	e, err := l.jobEnv(v.env)
	if err != nil {
		return notStarted(err, stripHostPaths(err.Error(), noAppDirs()))
	}
	app, err := l.newApp(v.env)
	if err != nil {
		return notStarted(err, stripHostPaths(err.Error(), noAppDirs()))
	}
	_, err = l.startJob(e, app, daemonproto.JobInfo{Kind: daemonproto.JobVolumeResize}, func(j *envJob, out *cappedlog.Log) {
		l.runVolumeJob(e, j, app, out, r)
	})
	if err != nil {
		return notStarted(err, app.sandboxText(err.Error()))
	}
	return nil
}

// resizeNotStarted is a resize that did not start: the error, and its
// text as the sandbox may read it (broker.SandboxTexter).
type resizeNotStarted struct {
	err     error
	sandbox string
}

func (e resizeNotStarted) Error() string       { return e.err.Error() }
func (e resizeNotStarted) Unwrap() error       { return e.err }
func (e resizeNotStarted) SandboxText() string { return e.sandbox }

var _ broker.SandboxTexter = resizeNotStarted{}

func notStarted(err error, sandbox string) error {
	return resizeNotStarted{err: err, sandbox: sandbox}
}

// noAppDirs are the host's directories a failure's text may name before
// there is an environment's App to say which: the user's home and the
// caboose dirs the environment names.
func noAppDirs() []string {
	home, _ := os.UserHomeDir()
	return []string{os.Getenv("CABOOSE_DATA_DIR"), os.Getenv("CABOOSE_HOME"), home}
}

// runVolumeJob makes request r's resize on app, records what came of it
// on the request, and ends the job. The request's record is the
// sandbox's to read, so a failure is recorded there as one line with the
// host's paths left out (sandboxText); the whole of it goes to jobs.log
// and the notification, for the person.
func (l *DaemonLinks) runVolumeJob(e *envLink, j *envJob, app *App, out *cappedlog.Log, r broker.Resize) {
	env, id, name, size := e.name, r.ID, r.Volume, r.Size
	lg := log.New(out, "", log.LstdFlags)
	lg.Printf("%s %s: request #%d: volume %s to %s", j.info.Kind, env, id, name, sizeValue(size))
	jobOutput(app, out)
	app.noTerminal = fmt.Errorf("this needs a terminal: run `%s` on the host", envCommand(env, "volume resize "+name+" "+sizeValue(size)))
	outcome, err := l.volumeRun(app, r)
	if err != nil {
		outcome = "the resize failed: " + app.sandboxText(err.Error())
	}
	if h, herr := l.brokerOf(env); herr != nil {
		lg.Printf("request #%d: what came of it is not recorded on the request (%v): %s", id, herr, outcome)
	} else if rerr := h.b.RecordOutcome(id, outcome); rerr != nil {
		lg.Printf("request #%d: %v", id, rerr)
	}
	text := fmt.Sprintf("request #%d: %s", id, outcome)
	if err != nil {
		// endJob appends the job's error, err in a line, to text.
		text = fmt.Sprintf("request #%d: resizing volume %s failed: ", id, name)
	}
	l.endJob(e, j, out, lg, err, nil, text)
}

// volumeRun is the job's resize on app; a panic is the job's failure, not
// the daemon's. The daemon's stopping interrupts it where that is safe.
func (l *DaemonLinks) volumeRun(app *App, r broker.Resize) (outcome string, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("internal error: %v", p)
		}
	}()
	if l.volumeAction != nil {
		return l.volumeAction(app, r)
	}
	return app.requestedResize(l.ctx, r)
}

// sandboxText is s, a failure's text, as the sandbox may read it: one
// line, each path under this machine's caboose dirs or the user's home
// (a disk's, config.toml's) replaced by "(a path on the host)".
func (a *App) sandboxText(s string) string {
	c := a.Cfg
	return stripHostPaths(s, []string{c.DataDir, c.EnvDir, c.CabooseHome, c.Home})
}

// stripHostPaths is s in one line, each path under one of dirs replaced
// by "(a path on the host)".
func stripHostPaths(s string, dirs []string) string {
	var alts []string
	for _, d := range dirs {
		d = strings.TrimRight(d, "/")
		if d != "" {
			alts = append(alts, regexp.QuoteMeta(d))
		}
	}
	s = oneLine(s)
	if len(alts) == 0 {
		return s
	}
	// The path runs to a space, a quote or a bracket, and leaves out
	// the punctuation that ends a clause.
	re := regexp.MustCompile(`(?:` + strings.Join(alts, "|") + `)(?:/[^\s'"(),;]*[^\s'"(),;:.])?`)
	return re.ReplaceAllString(s, "(a path on the host)")
}

// requestedResize makes the resize a person allowed: request r's volume
// of the vm profile in use to r.Size, while the VM runs, as caboose volume
// resize does, and only so. config.toml and the VM are looked at again
// first: what changed since the request was filed (the volume removed,
// volume_limit lowered, another vm profile, the volume's size in
// config.toml or its disk's not the one the request was filed against,
// the VM stopped or recreated without the volume on caboose-vmm's disk)
// refuses it, changing nothing. outcome says what was done, in a line.
// Once config.toml has the new size, sessions are told
// (exportProposals).
func (a *App) requestedResize(ctx context.Context, r broker.Resize) (outcome string, err error) {
	name, size := r.Volume, r.Size
	c := a.Cfg
	if !a.isVM() {
		return "", fmt.Errorf("the environment's isolation is %s now, and only a vm profile's volumes have a size", isolationOf(c))
	}
	p := policy.Policy{VMProfile: c.Profile, VolumeLimit: c.VMVolumeLimit, Volumes: map[string]int64{}}
	for _, v := range c.Volumes {
		p.Volumes[v.Name] = v.Size
	}
	if _, why := policy.VolumeResizeRefusal(p, name, size); why != "" {
		return "", errors.New(why)
	}
	if c.Profile != r.Profile || p.Volumes[name] != r.Configured || a.volumeDiskSize(name, 0) != r.From {
		return "", errors.New(broker.VolumeChanged)
	}
	vr, ok := a.box().(backend.VolumeResizer)
	if !ok {
		return "", errors.New("this sandbox's volumes cannot be resized")
	}
	vs, err := vr.VolumeState(name)
	if err != nil {
		return "", err
	}
	if !vs.Running || !vs.Has || !vs.Live {
		return "", fmt.Errorf("the VM no longer runs with volume %s on one of caboose-vmm's disks: nothing was changed", name)
	}
	edit := config.Edit{Set: map[string]any{c.Profile + ".volume." + name + ".size": sizeValue(size)}}
	if err := a.checkConfigEdit(edit); err != nil {
		return "", err
	}
	res, err := a.liveResize(ctx, vr, name, size, edit, volumeTable(c, name))
	if res.Applied {
		a.exportResized()
	}
	if err != nil {
		if res.Applied {
			return "", fmt.Errorf("%w (the disk is %s now, and config.toml says so)", err, sizeValue(size))
		}
		return "", err
	}
	return fmt.Sprintf("volume %s is %s now (it was %s), and config.toml says so", name, sizeValue(size), gibString(res.From)), nil
}

// exportResized tells sessions what a proposal is made against again
// (exportProposals), from config.toml as a resize just left it.
func (a *App) exportResized() {
	if err := a.reloadConfig(); err != nil {
		a.Note("not telling sessions the volume's new size: %s", oneLine(err.Error()))
		return
	}
	a.exportProposals()
}
