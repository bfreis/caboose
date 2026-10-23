// Package broker keeps one environment's policy at work: it builds a
// policy.Policy from config.toml, the grants in force and what the
// sandboxes can write, decides each crossing with it, files the crossings
// that ask as requests a person answers, and turns those answers into
// grants or config.toml edits.
//
// Its state lives in the environment's directory, which no sandbox
// mounts: grants.json, requests.json, decisions.log and broker.lock.
// Every read and write of them goes through internal/nofollow, and every
// read-modify-write of grants.json and requests.json happens under an
// exclusive flock on broker.lock, re-reading both inside it, so two
// writers never lose a grant or reuse a number.
//
// Nothing the sandbox can reach -- Decide, File, List, Wait -- adds or
// extends a grant, edits config.toml or decides a request: only Answer
// and expiry do. Consuming a once grant is the one change a crossing
// makes to the grants.
package broker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/nofollow"
	"github.com/bfreis/caboose/internal/policy"
)

// The files the broker keeps in the environment's directory.
const (
	grantsFile    = "grants.json"
	requestsFile  = "requests.json"
	decisionsFile = "decisions.log"
	lockFile      = "broker.lock"
)

// Limits and timings.
const (
	// MaxPending is the most requests pending at once in an environment.
	MaxPending = 100
	// MaxNewPerMinute is the most new requests filed in a minute; a
	// request merged into a pending one does not count.
	MaxNewPerMinute = 10
	// RequestTTL is how long a request waits for an answer.
	RequestTTL = 24 * time.Hour
	// OnceTTL is how long a once grant waits to be used.
	OnceTTL = 15 * time.Minute
	// MaxHours is the longest an "hours" answer may grant.
	MaxHours = 168
	// AttemptsEvery is how often at most a repeat of a pending request
	// rewrites requests.json to count it: the repeats in between are
	// counted in memory and written with the next write.
	AttemptsEvery = 10 * time.Second
	// KeepDecided is how many decided requests requests.json keeps.
	KeepDecided = 200
	// MaxList is the most requests List returns.
	MaxList = 200
	// MaxWaitsPerLink is the most Waits one link may have at once.
	MaxWaitsPerLink = 16
	// MaxAliases is the most aliases Aliases lists.
	MaxAliases = 256

	maxStateFile = 8 << 20  // grants.json, requests.json
	maxDecisions = 10 << 20 // decisions.log, then decisions.log.1
)

// Error is a broker error with a stable kind, for the daemon's protocol.
type Error struct {
	Kind string
	Msg  string
}

func (e *Error) Error() string { return e.Msg }

// Is matches any *Error of the same kind, so errors.Is(err, ErrNotFound)
// holds for every not-found error whatever it says.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t.Kind == e.Kind
}

// The error kinds.
const (
	KindNotOwner        = "not_owner"
	KindNotFound        = "not_found"
	KindAlreadyAnswered = "already_answered"
	KindStaleDiff       = "stale_diff"
	KindTooManyWaits    = "too_many_waits"
	KindBadRequest      = "bad_request"
	KindUnavailable     = "unavailable"
)

// Sentinels for errors.Is; the errors returned carry their own text.
var (
	ErrNotOwner        = &Error{KindNotOwner, "this daemon does not hold the environment's link.lock, so it does not serve its broker"}
	ErrNotFound        = &Error{KindNotFound, "no such request"}
	ErrAlreadyAnswered = &Error{KindAlreadyAnswered, "already answered"}
	ErrStaleDiff       = &Error{KindStaleDiff, "config.toml would change differently than the diff confirmed"}
	ErrTooManyWaits    = &Error{KindTooManyWaits, "too many waits"}
	ErrBadRequest      = &Error{KindBadRequest, "bad request"}
	ErrUnavailable     = &Error{KindUnavailable, "unavailable"}
)

func newError(kind, format string, args ...any) *Error {
	return &Error{Kind: kind, Msg: fmt.Sprintf(format, args...)}
}

// Owner says whether this process serves the environment's broker.
type Owner interface {
	// Owns is nil while this process owns the environment, ErrNotOwner
	// (or another error) otherwise.
	Owns() error
}

// LockOwner is the Owner for the process holding f, its open
// link.lock: an exclusive non-blocking flock on f, which is a no-op for
// the holder and fails for anyone else. A nil f owns nothing.
func LockOwner(f *os.File) Owner { return lockOwner{f} }

type lockOwner struct{ f *os.File }

func (o lockOwner) Owns() error {
	if o.f == nil {
		return ErrNotOwner
	}
	err := flock(int(o.f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return ErrNotOwner
	}
	if err != nil {
		return newError(KindNotOwner, "cannot check the environment's link.lock: %v", err)
	}
	return nil
}

// flock is unix.Flock on fd, again when a signal interrupts it.
func flock(fd, how int) error {
	for {
		err := unix.Flock(fd, how)
		if !errors.Is(err, syscall.EINTR) {
			return err
		}
	}
}

// Settings is what the broker reads of the environment's configuration.
type Settings struct {
	// Config is the environment's configuration, as config.Load gives it.
	Config *config.Config
	// VMProfile is the name of the vm profile the sandbox runs under
	// ("default" for [vm.default]), "" when it runs under none: where an
	// "always" answer to an egress request writes egress_allow.
	VMProfile string
	// ConfigPath is config.toml's path, for what answers say.
	ConfigPath string
}

// Source reads the environment's configuration again.
type Source func() (Settings, error)

// ConfigWriter edits config.toml for an "always" answer.
type ConfigWriter interface {
	// Diff is the change e would make to config.toml, as the person
	// confirming it is shown it; nothing is written.
	Diff(e config.Edit) (string, error)
	// Write makes e, provided the change it makes is still the one whose
	// Diff hashes (DiffHash) to diffHash.
	Write(e config.Edit, diffHash string) error
}

// Opener opens a URL in this machine's browser.
type Opener interface {
	OpenURL(url string) error
}

// Dialog asks a person about a URL request as soon as it is filed. Ask
// must not block: whoever implements it shows the dialog later, and
// answers with Broker.Answer, By "dialog" (or the menu-bar app does, By
// "app"). While a dialog shows, the request is marked in flight
// (MarkInFlight): a broker that opens to find a pending request so
// marked denies it, since whoever showed it went away.
type Dialog interface {
	Ask(env string, id uint64, url string)
}

// Options is how Open sets a Broker up.
type Options struct {
	// Home is CABOOSE_HOME and Env the environment: the state lives in
	// config.EnvDirFor(Home, Env).
	Home, Env string
	// Owner says whether this process serves the environment; nil owns
	// nothing, and Open refuses it.
	Owner Owner
	// Source reads the configuration; required.
	Source Source
	// Writable are the physical host paths some sandbox can write right
	// now, read off the sandboxes' real mounts. An error, or a nil
	// Writable, counts everything as writable ("/"). It is read at every
	// Reload, every minute, and before an alias is decided when what was
	// read is older than a couple of seconds.
	Writable func() ([]string, error)
	// ConfigWriter writes "always" answers; nil refuses them.
	ConfigWriter ConfigWriter
	// Opener opens an allowed URL request's URL; nil refuses allowing one.
	Opener Opener
	// Dialog is told of each new URL request; nil leaves it pending.
	Dialog Dialog
	// Now is the clock; nil for time.Now.
	Now func() time.Time
	// Log is for what the host's operator should see (unusable files,
	// dropped dialog answers); nil discards it.
	Log *log.Logger
	// RequiredHosts are the hosts a hostile sandbox cannot start without
	// (Claude Code's own), which a launch refuses unless egress_allow
	// names them: under any trust but fallible, EditEgress refuses an
	// edit that would leave one of them unallowed or denied. Nil refuses
	// every removal from egress_allow and every addition to egress_deny
	// there, since what they would cut off cannot be told.
	RequiredHosts []string
}

// Broker is one environment's broker.
type Broker struct {
	opts   Options
	dir    nofollow.Dir
	envDir string
	now    func() time.Time
	logf   func(format string, args ...any)

	// fileMu serialises this Broker's read-modify-writes; the flock on
	// lock serialises them with other Brokers and processes. flock
	// belongs to the open file, so goroutines sharing lock need fileMu.
	fileMu sync.Mutex
	lock   *os.File
	// attempts are the repeats of pending requests not yet in
	// requests.json, by request; under fileMu (heldAttempts).
	attempts map[uint64]*heldAttempts

	// writableMu is held while the writable set is read again
	// (refreshWritable).
	writableMu sync.Mutex

	mu         sync.Mutex // guards what follows
	writableAt time.Time  // when pol's writable set was read
	pol        policy.Policy
	settings   Settings
	st         state
	cond       chan struct{} // closed and replaced when seq moves

	dlog *decisionLog

	// limits, fields so tests can lower them.
	maxPending, maxPerMinute int
}

// Open opens env's broker. It refuses a nil Owner, a process that does
// not own the environment, and a Source that fails.
func Open(o Options) (*Broker, error) {
	if o.Owner == nil {
		return nil, newError(KindNotOwner, "the broker has no owner check: refused")
	}
	if err := o.Owner.Owns(); err != nil {
		return nil, err
	}
	if o.Source == nil {
		return nil, fmt.Errorf("broker: no configuration source")
	}
	if !config.ValidEnv(o.Env) || o.Home == "" {
		return nil, fmt.Errorf("broker: bad environment %q or CABOOSE_HOME %q", o.Env, o.Home)
	}
	b := &Broker{
		opts:         o,
		envDir:       config.EnvDirFor(o.Home, o.Env),
		now:          o.Now,
		cond:         make(chan struct{}),
		maxPending:   MaxPending,
		maxPerMinute: MaxNewPerMinute,
	}
	if b.now == nil {
		b.now = time.Now
	}
	l := o.Log
	if l == nil {
		l = log.New(io.Discard, "", 0)
	}
	b.logf = func(format string, args ...any) { l.Printf("broker %s: "+format, append([]any{o.Env}, args...)...) }
	b.dir = nofollow.Dir(b.envDir)
	if err := os.MkdirAll(b.envDir, 0o700); err != nil {
		return nil, err
	}
	fd, err := unix.Open(b.envDir+"/"+lockFile, unix.O_RDWR|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("broker: %s/%s: %w", b.envDir, lockFile, err)
	}
	b.lock = os.NewFile(uintptr(fd), b.envDir+"/"+lockFile)
	if fi, err := b.lock.Stat(); err != nil || !fi.Mode().IsRegular() {
		b.lock.Close()
		return nil, fmt.Errorf("broker: %s/%s is not a plain file", b.envDir, lockFile)
	}
	b.dlog = openDecisionLog(b.dir, b.envDir, b.logf)
	b.mu.Lock()
	b.st = b.readState()
	b.mu.Unlock()
	if err := b.Reload(); err != nil {
		b.Close()
		return nil, err
	}
	b.denyOrphans()
	return b, nil
}

// Close releases the broker's files.
func (b *Broker) Close() error {
	b.dlog.close()
	return b.lock.Close()
}

// owns is the owner check every serving method makes first.
func (b *Broker) owns() error { return b.opts.Owner.Owns() }

// Run expires requests and writes the hourly egress counts, every
// minute, until ctx ends.
func (b *Broker) Run(ctx context.Context) {
	t := time.NewTicker(runEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if b.owns() != nil {
				continue
			}
			b.expire()
			b.dlog.flushEgress(b.now(), false)
			b.refreshWritable(runEvery)
		}
	}
}

// runEvery is how often Run looks for expired requests; a variable for
// the tests.
var runEvery = time.Minute
