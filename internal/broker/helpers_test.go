package broker

import (
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/policy"
)

const testEnv = "t"

// owner is an Owner the test controls.
type owner struct{ lost atomic.Bool }

func (o *owner) Owns() error {
	if o.lost.Load() {
		return ErrNotOwner
	}
	return nil
}

// clock is a settable clock.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// source serves a config the test changes.
type source struct {
	mu  sync.Mutex
	cfg config.Config
	err error
}

func (s *source) get() (Settings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return Settings{}, s.err
	}
	c := s.cfg
	c.Aliases = slices.Clone(c.Aliases)
	return Settings{Config: &c, ConfigPath: "/h/.caboose/envs/t/config.toml"}, nil
}

func (s *source) set(f func(c *config.Config)) {
	s.mu.Lock()
	f(&s.cfg)
	s.mu.Unlock()
}

// writer is a ConfigWriter that applies its edits to a source.
type writer struct {
	mu     sync.Mutex
	src    *source
	drift  string // added to every diff: the file changed under the answer
	diffs  int
	writes []config.Edit
}

func editText(e config.Edit) string {
	var b strings.Builder
	for _, k := range slices.Sorted(maps.Keys(e.Set)) {
		fmt.Fprintf(&b, "+%s = %v\n", k, e.Set[k])
	}
	return b.String()
}

func (w *writer) Diff(e config.Edit) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.diffs++
	return editText(e) + w.drift, nil
}

func (w *writer) Write(e config.Edit, hash string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if DiffHash(editText(e)+w.drift) != hash {
		return ErrStaleDiff
	}
	w.writes = append(w.writes, e)
	w.src.set(func(c *config.Config) {
		for k, v := range e.Set {
			switch {
			case strings.HasPrefix(k, "alias.") && strings.HasSuffix(k, ".policy"):
				name := strings.TrimSuffix(strings.TrimPrefix(k, "alias."), ".policy")
				for i := range c.Aliases {
					if c.Aliases[i].Name == name {
						c.Aliases[i].Policy = v.(string)
					}
				}
			case strings.HasSuffix(k, ".egress_allow"):
				c.EgressAllow = v.([]string)
			case strings.HasSuffix(k, ".egress_deny"):
				c.EgressDeny = v.([]string)
			case k == "link.open_urls_allow":
				c.OpenURLsAllow = v.([]string)
			case k == "link.forward_ports":
				var parts []string
				for _, e := range v.([]any) {
					parts = append(parts, fmt.Sprint(e))
				}
				c.ForwardPorts = strings.Join(parts, " ")
				if len(parts) == 0 {
					c.ForwardPorts = "none"
				}
			}
		}
	})
	return nil
}

func (w *writer) calls() (int, int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.diffs, len(w.writes)
}

type opener struct {
	mu   sync.Mutex
	urls []string
}

func (o *opener) OpenURL(u string) error {
	o.mu.Lock()
	o.urls = append(o.urls, u)
	o.mu.Unlock()
	return nil
}

func (o *opener) opened() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return slices.Clone(o.urls)
}

type dialog struct {
	mu  sync.Mutex
	ids []uint64
}

func (d *dialog) Ask(env string, id uint64, url string) {
	d.mu.Lock()
	d.ids = append(d.ids, id)
	d.mu.Unlock()
}

// fixture is a broker on a temp env dir with fakes around it.
type fixture struct {
	t      *testing.T
	home   string
	dir    string
	b      *Broker
	owner  *owner
	clock  *clock
	src    *source
	w      *writer
	open   *opener
	dialog *dialog
	// required is Options.RequiredHosts for the next openBroker.
	required []string
}

func baseConfig() config.Config {
	return config.Config{
		Env: testEnv, Trust: config.TrustFallible, OpenURLs: "ask",
		Isolation: config.KindVM, Profile: "vm.default",
		Egress: true, EgressPorts: config.DefaultEgressPorts,
		Aliases: []config.Alias{
			{Name: "build", Command: []string{"/usr/bin/make"}, Policy: config.AliasAsk},
			{Name: "free", Command: []string{"/usr/bin/true"}},
		},
	}
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	home := t.TempDir()
	f := &fixture{t: t, home: home, dir: config.EnvDirFor(home, testEnv), owner: &owner{}, clock: newClock(),
		src: &source{cfg: baseConfig()}, open: &opener{}, dialog: &dialog{}}
	f.w = &writer{src: f.src}
	f.b = f.openBroker()
	return f
}

func (f *fixture) options() Options {
	return Options{
		Home: f.home, Env: testEnv, Owner: f.owner, Source: f.src.get,
		Writable:     func() ([]string, error) { return []string{"/work"}, nil },
		ConfigWriter: f.w, Opener: f.open, Dialog: f.dialog, Now: f.clock.Now,
		RequiredHosts: f.required,
	}
}

func (f *fixture) openBroker() *Broker {
	f.t.Helper()
	b, err := Open(f.options())
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { b.Close() })
	return b
}

// reopen closes the broker and opens another on the same dir.
func (f *fixture) reopen() {
	f.t.Helper()
	f.b.Close()
	f.b = f.openBroker()
}

func (f *fixture) read(name string) []byte {
	f.t.Helper()
	data, err := os.ReadFile(f.dir + "/" + name)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		f.t.Fatal(err)
	}
	return data
}

func (f *fixture) decide(r policy.Request) Outcome {
	f.t.Helper()
	o, err := f.b.Decide(r)
	if err != nil {
		f.t.Fatal(err)
	}
	return o
}

func (f *fixture) answer(id uint64, rp Reply) AnswerResult {
	f.t.Helper()
	if rp.By == "" {
		rp.By = ByCLI
	}
	res, err := f.b.Answer(id, rp)
	if err != nil {
		f.t.Fatalf("answer #%d %+v: %v", id, rp, err)
	}
	return res
}

func aliasReq(name string) policy.Request {
	cmd := "/usr/bin/make"
	if name == "free" {
		cmd = "/usr/bin/true"
	}
	// An identity no file has, as the caller would find the command's.
	return policy.Request{Kind: policy.KindAlias, Alias: name, Command: cmd, CommandIDs: []policy.FileID{{Dev: 1 << 62, Ino: 1}}}
}

// egressReq is a connection to host at addr, port 443.
func egressReq(host, addr string) policy.Request {
	return policy.Request{Kind: policy.KindEgress, Host: host, Port: 443, Addrs: []netip.Addr{netip.MustParseAddr(addr)}}
}

func urlReq(u string) policy.Request {
	origin := u
	if i := strings.Index(u[len("https://"):], "/"); i >= 0 {
		origin = u[:len("https://")+i]
	}
	return policy.Request{Kind: policy.KindOpenURL, URL: u, Origin: origin}
}

func mustPending(t *testing.T, o Outcome) uint64 {
	t.Helper()
	if o.Verdict != Pending || o.Request == 0 {
		t.Fatalf("want pending, got %+v", o)
	}
	return o.Request
}

// portReq is port, as the link asks about it once the host probed it
// and found no program of its own there.
func portReq(port int) policy.Request {
	return policy.Request{Kind: policy.KindPort, Port: port, HostChecked: true}
}
