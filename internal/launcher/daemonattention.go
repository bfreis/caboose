package launcher

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bfreis/caboose/internal/assets"
	"github.com/bfreis/caboose/internal/checkpoint"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/daemon"
	"github.com/bfreis/caboose/internal/daemonproto"
	"github.com/bfreis/caboose/internal/proposal"
	"github.com/bfreis/caboose/internal/selfupdate"
)

// What needs the user (status.attention), from the daemon's own state:
// the daemon calls these from its poll loop only, never for a status.
// Every text is written here; what came from a sandbox (an error that
// quotes it) is made printable and cut short first.

// attentionSeams are what the sources read that a test replaces.
type attentionSeams struct {
	now func() time.Time
	// version is the running daemon's.
	version string
	// imageNote is what is wrong with env's image, "" when nothing; it
	// asks the engine.
	imageNote func(ctx context.Context, app *App) (string, error)
	// updateState is the self-update record.
	updateState func(home string) selfupdate.State
}

// daemonAttention is the daemon.Attention over a DaemonLinks.
type daemonAttention struct {
	l *DaemonLinks
	s attentionSeams

	mu       sync.Mutex
	dataDirs map[string]string // env: its data dir, once known
}

var _ daemon.Attention = (*daemonAttention)(nil)

// NewDaemonAttention is the daemon's sources of what needs the user.
func NewDaemonAttention(l *DaemonLinks, version string) daemon.Attention {
	return newDaemonAttention(l, attentionSeams{now: time.Now, version: version,
		imageNote: imageNote, updateState: selfupdate.ReadState})
}

func newDaemonAttention(l *DaemonLinks, s attentionSeams) *daemonAttention {
	return &daemonAttention{l: l, s: s, dataDirs: map[string]string{}}
}

// envNames are the environments under CABOOSE_HOME.
func (d *daemonAttention) envNames() []string {
	if d.l.home == "" {
		return nil
	}
	entries, err := os.ReadDir(filepath.Join(d.l.home, "envs"))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() && config.ValidEnv(e.Name()) {
			out = append(out, e.Name())
		}
	}
	slices.Sort(out)
	return out
}

// SlowEnvs is every environment.
func (d *daemonAttention) SlowEnvs() []string { return d.envNames() }

func attnCount(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

// clip makes s, which may be the sandbox's, printable and short.
func clip(s string, n int) string { return daemonproto.Text(proposal.Printable(s), n) }

// Cheap is the kinds computed from the daemon's own state and the files
// it lists.
func (d *daemonAttention) Cheap(ctx context.Context) []daemon.Finding {
	var out []daemon.Finding
	l := d.l
	l.mu.Lock()
	links := make([]*envLink, 0, len(l.envs))
	for _, e := range l.envs {
		links = append(links, e)
	}
	l.mu.Unlock()
	for _, e := range links {
		st := e.status()
		route := daemonproto.RouteEnv(e.name)
		if st.Pending > 0 {
			out = append(out, daemon.Finding{Env: e.name, Kind: daemonproto.AttentionRequests,
				Text: attnCount(st.Pending, "pending request", "pending requests"), Route: daemonproto.RouteEnvRequests(e.name)})
		}
		if st.Link == daemonproto.LinkFailed {
			text := "The link failed"
			if st.LastError != "" {
				text += ": " + clip(st.LastError, 200)
			}
			out = append(out, daemon.Finding{Env: e.name, Kind: daemonproto.AttentionLinkFailed, Text: text, Route: route})
		}
		e.mu.Lock()
		cur := e.cur
		e.mu.Unlock()
		if cur != nil {
			if w := cur.runner.vnodeWarning(); w != "" {
				out = append(out, daemon.Finding{Env: e.name, Kind: daemonproto.AttentionVnodes, Text: w, Route: route})
			}
		}
		if text := checkpointAttention(l.schedStatus(e.name)); text != "" {
			out = append(out, daemon.Finding{Env: e.name, Kind: daemonproto.AttentionCheckpoint, Text: text, Route: route})
		}
	}
	out = append(out, d.proposals()...)
	return out
}

// checkpointAttention is what is wrong with an environment's checkpoints,
// or "".
func checkpointAttention(st *checkpoint.Status) string {
	if st == nil {
		return ""
	}
	var parts []string
	if st.Exposed != "" {
		parts = append(parts, "the checkpoint store can be reached by the sandbox: "+clip(st.Exposed, 100))
	}
	if st.LastError != "" {
		parts = append(parts, "the last checkpoint failed: "+clip(st.LastError, 100))
	}
	switch {
	case st.SpaceSkipped != "":
		parts = append(parts, "checkpoints skipped: low disk space")
	case st.LowSpace:
		parts = append(parts, "low disk space for checkpoints")
	}
	for _, p := range st.Paths {
		if p.Shrunk != "" {
			parts = append(parts, "a checkpointed path shrank sharply")
			break
		}
	}
	if len(parts) == 0 {
		return ""
	}
	s := strings.Join(parts, "; ")
	return strings.ToUpper(s[:1]) + s[1:]
}

// dataDir is env's data dir, from its config.toml, once.
func (d *daemonAttention) dataDir(env string) (string, bool) {
	d.mu.Lock()
	dir, ok := d.dataDirs[env]
	d.mu.Unlock()
	if ok {
		return dir, true
	}
	if d.l.newApp == nil {
		return "", false
	}
	app, err := d.l.newApp(env)
	if err != nil {
		return "", false
	}
	d.mu.Lock()
	d.dataDirs[env] = app.Cfg.DataDir
	d.mu.Unlock()
	return app.Cfg.DataDir, true
}

// proposals counts each environment's pending proposal files, listing the
// directory the sandbox writes through nofollow and reading nothing.
func (d *daemonAttention) proposals() []daemon.Finding {
	var out []daemon.Finding
	for _, env := range d.envNames() {
		dir, ok := d.dataDir(env)
		if !ok {
			continue
		}
		stamps, err := proposal.Stamps(dir)
		if err != nil || len(stamps) == 0 {
			continue
		}
		out = append(out, daemon.Finding{Env: env, Kind: daemonproto.AttentionProposals,
			Text: attnCount(len(stamps), "pending proposal", "pending proposals"), Route: daemonproto.RouteProposals})
	}
	return out
}

// Slow is env's image_stale.
func (d *daemonAttention) Slow(ctx context.Context, env string) ([]daemon.Finding, error) {
	if d.l.newApp == nil {
		return nil, nil
	}
	app, err := d.l.newApp(env)
	if err != nil {
		return nil, err
	}
	text, err := d.s.imageNote(ctx, app)
	if err != nil || text == "" {
		return nil, err
	}
	return []daemon.Finding{{Env: env, Kind: daemonproto.AttentionImageStale, Text: text, Route: daemonproto.RouteEnv(env)}}, nil
}

// imageNote asks the engine, and says what is wrong with app's image:
// built by another caboose, or on another base than the profile names.
// The engine's calls take no context, so they run on a goroutine that is
// left behind when ctx ends.
func imageNote(ctx context.Context, app *App) (string, error) {
	type asked struct {
		text string
		err  error
	}
	ch := make(chan asked, 1)
	go func() {
		text, err := imageNoteNow(app)
		ch <- asked{text, err}
	}()
	select {
	case r := <-ch:
		return r.text, r.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func imageNoteNow(app *App) (string, error) {
	labels, exists, err := app.images().ImageLabels(app.Cfg.Image)
	if err != nil && !exists {
		return "", err
	}
	st := app.imageStatus(labels, exists)
	if st.state != imageStale {
		return "", nil
	}
	if st.switched() {
		return "The image was built on another base than the configured one; a restart rebuilds it", nil
	}
	by := labels[assets.LabelVersion]
	if by == "" {
		by = "an unknown caboose"
	}
	return "The image is out of date (built by " + clip(by, 40) + "): a restart moves onto a new one", nil
}

// Update is the update kind: the self-update record names a release newer
// than this daemon.
func (d *daemonAttention) Update(context.Context) ([]daemon.Finding, error) {
	cur := d.s.version
	if d.l.dev || d.l.home == "" || !selfupdate.Valid(cur) {
		return nil, nil
	}
	st := d.s.updateState(d.l.home)
	if st.Latest == "" || !selfupdate.Valid(st.Latest) || selfupdate.Compare(st.Latest, cur) <= 0 {
		return nil, nil
	}
	return []daemon.Finding{{Kind: daemonproto.AttentionUpdate,
		Text: "caboose " + st.Latest + " is out (this is " + cur + ")"}}, nil
}
