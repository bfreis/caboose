package launcher

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/bfreis/caboose/internal/agentproto"
	"github.com/bfreis/caboose/internal/broker"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/hostlink"
)

// openBroker opens the environment's request broker for this link, which
// holds lock, the environment's link.lock: the broker serves only while
// it is held (broker.LockOwner), so only the process running the
// environment's link ever decides for it, files its requests or answers
// them. Its policy is config.toml's, read again whenever the link sees
// the file change, and the grants its requests' answers made.
func (r *linkRunner) openBroker(lock *os.File) (*broker.Broker, error) {
	q := r.dialogs
	if q == nil {
		q = newDialogQueue()
	}
	d := &urlDialog{actions: r.cfg.Actions, logf: r.log.Printf, queue: q}
	b, err := broker.Open(r.brokerOptions(lock, d))
	if err != nil {
		return nil, err
	}
	d.answers = b
	r.dialog = d
	return b, nil
}

// brokerSettings is what the broker reads of c: the vm profile an "always"
// egress answer writes to, and the config.toml those answers edit.
func brokerSettings(c *config.Config, envDir string) broker.Settings {
	s := broker.Settings{Config: c, ConfigPath: filepath.Join(envDir, config.FileName)}
	if c.File != nil && c.File.Path != "" {
		s.ConfigPath = c.File.Path
	}
	if name, ok := strings.CutPrefix(c.Profile, config.KindVM+"."); ok && c.Isolation == config.KindVM {
		s.VMProfile = name
	}
	return s
}

// closeBroker closes the broker, once no dialog can answer through it
// any more: the caller lets go of link.lock next, and a dialog still
// showing must not answer for an environment this process no longer
// owns.
func (r *linkRunner) closeBroker() {
	if r.broker == nil {
		return
	}
	r.dialog.close()
	if r.dialogs == nil {
		// The link's own queue (no daemon shares it): it ends with it.
		r.dialog.queue.close()
	}
	r.broker.Close()
}

// setMods keeps the agent's last count of Claude Code sessions and of
// those with caboose's mod.
func (r *linkRunner) setMods(m agentproto.ModsInfo) {
	r.mu.Lock()
	r.mods = &m
	r.mu.Unlock()
}

// lastMods is the agent's last count, and whether it sent one.
func (r *linkRunner) lastMods() (agentproto.ModsInfo, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.mods == nil {
		return agentproto.ModsInfo{}, false
	}
	return *r.mods, true
}

// urlAnswers is what a URL dialog needs of the broker.
type urlAnswers interface {
	Request(id uint64) (broker.RequestInfo, error)
	Answer(id uint64, rp broker.Reply) (broker.AnswerResult, error)
	MarkInFlight(id uint64, by string) error
}

// urlDialog is the broker's Dialog: it queues each URL request as it is
// filed (dialogQueue), whose dialog asks whoever is at this machine, one
// dialog at a time across the process's links, and answers the request
// with what they said. The agent that asked is not kept waiting: it was
// told the request is pending, and the broker opens the URL once it is
// allowed. A request answered meanwhile (by the CLI) shows no dialog;
// one whose dialog cannot be shown stays pending.
type urlDialog struct {
	actions hostlink.Actions
	answers urlAnswers
	logf    func(format string, args ...any)
	queue   *dialogQueue

	mu     sync.Mutex // held while it answers
	closed bool       // the link is done: show nothing, answer nothing
}

// close ends the dialogs: none shows after it, and none showing answers.
func (d *urlDialog) close() {
	d.mu.Lock()
	d.closed = true
	d.mu.Unlock()
	if d.queue != nil {
		d.queue.drop(d)
	}
}

// serving runs f while the dialogs still serve, and reports whether
// they did: close waits for it, so nothing f does reaches the broker
// after close returns.
func (d *urlDialog) serving(f func()) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return false
	}
	f()
	return true
}

func (d *urlDialog) Ask(env string, id uint64, url string) {
	d.queue.push(&dialogItem{d: d, env: env, id: id, url: url})
}

// show shows request id's dialog on this machine, if it still waits:
// the queue calls it, one at a time. The request is in flight with the
// dialog while it shows (marked so in requests.json): a broker that
// opens to find it so, because this one went away meanwhile, denies it.
func (d *urlDialog) show(env string, id uint64, url string) {
	if d.answers == nil || !d.pending(id) {
		return
	}
	// No way to ask on this machine: it stays pending, unmarked.
	if d.actions == nil {
		d.logf("request #%d: no dialog on this machine: it stays pending for caboose requests", id)
		return
	}
	// A dialog no mark records would be asked again, another way, if the
	// daemon went away while it shows: none shows.
	if !d.mark(id, broker.ByDialog, "no dialog; it stays pending for caboose requests") {
		return
	}
	ok, err := d.actions.Confirm(fmt.Sprintf("The caboose sandbox of environment %s asks to open this URL in your browser (request #%d):\n\n%s", env, id, url))
	if err != nil {
		d.logf("request #%d: cannot ask on this machine (%v): it stays pending for caboose requests", id, err)
		// No dialog showed: nothing to deny if the daemon goes away.
		d.mark(id, "", "it stays marked, and is denied if the daemon goes away")
		return
	}
	rp := broker.Reply{Verdict: broker.ReplyDeny, Scope: broker.ScopeOnce, By: broker.ByDialog}
	if ok {
		rp.Verdict = broker.ReplyAllow
	}
	var res broker.AnswerResult
	if !d.serving(func() { res, err = d.answers.Answer(id, rp) }) {
		d.logf("request #%d: the dialog was answered after the link stopped: dropped; the next link denies it", id)
		return
	}
	switch {
	case errors.Is(err, broker.ErrAlreadyAnswered):
		d.logf("request #%d: the dialog's answer came after another: dropped", id)
	case err != nil:
		d.logf("request #%d: cannot record the dialog's answer: %v", id, err)
	case res.OpenError != "":
		d.logf("request #%d: allowed, but the URL did not open: %s", id, res.OpenError)
	}
}

// brokerOptions is how openBroker opens the environment's broker.
func (r *linkRunner) brokerOptions(lock *os.File, d *urlDialog) broker.Options {
	a := r.a
	return broker.Options{
		Home: a.Cfg.CabooseHome, Env: a.Cfg.Env, Owner: broker.LockOwner(lock),
		Source: func() (broker.Settings, error) {
			c, err := config.Load(a.Cfg.Getenv, config.OSFS{}, a.Cfg.Env)
			if err != nil {
				return broker.Settings{}, err
			}
			return brokerSettings(c, a.Cfg.EnvDir), nil
		},
		ConfigWriter: &configWriter{envDir: a.Cfg.EnvDir},
		// What any sandbox under CABOOSE_HOME can write now: no alias
		// runs a command within it.
		Writable: a.sandboxWritable,
		Opener:   r.cfg.Actions,
		Dialog:   d,
		Log:      r.log,
	}
}
