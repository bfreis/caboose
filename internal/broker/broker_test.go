package broker

import (
	"bytes"
	"context"
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
	"github.com/bfreis/caboose/internal/policy"
)

func TestOpenRefusesNilOwner(t *testing.T) {
	f := newFixture(t)
	o := f.options()
	o.Owner = nil
	if _, err := Open(o); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("nil owner: %v", err)
	}
	o.Owner = LockOwner(nil)
	if _, err := Open(o); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("nil link.lock: %v", err)
	}
}

// Only the process holding link.lock serves the broker: a second broker
// on the same environment is not_owner, at Open and on every call.
func TestSecondBrokerNotOwner(t *testing.T) {
	f := newFixture(t)
	lockPath := filepath.Join(f.dir, "link.lock")
	fa, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer fa.Close()
	fb, err := os.OpenFile(lockPath, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer fb.Close()

	oa := f.options()
	oa.Owner = LockOwner(fa)
	a, err := Open(oa)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if _, err := a.Decide(aliasReq("build")); err != nil {
		t.Fatalf("holder: %v", err)
	}

	ob := f.options()
	ob.Owner = LockOwner(fb)
	if _, err := Open(ob); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("second broker opened: %v", err)
	}
	var e *Error
	if _, err := Open(ob); !errors.As(err, &e) || e.Kind != KindNotOwner {
		t.Fatalf("kind: %v", err)
	}

	// A broker that loses ownership refuses every serving call.
	f.owner.lost.Store(true)
	b := f.b
	if o, err := b.Decide(aliasReq("build")); !errors.Is(err, ErrNotOwner) || o.Verdict != Denied {
		t.Errorf("Decide: %+v %v", o, err)
	}
	if _, err := b.File(Ask{Request: aliasReq("build")}); !errors.Is(err, ErrNotOwner) {
		t.Errorf("File: %v", err)
	}
	if _, _, err := b.List(Query{}); !errors.Is(err, ErrNotOwner) {
		t.Errorf("List: %v", err)
	}
	if _, err := b.Link().Wait(context.Background(), 0); !errors.Is(err, ErrNotOwner) {
		t.Errorf("Wait: %v", err)
	}
	if _, err := b.Answer(1, Reply{Verdict: ReplyDeny, By: ByCLI}); !errors.Is(err, ErrNotOwner) {
		t.Errorf("Answer: %v", err)
	}
	if _, err := b.Grants(); !errors.Is(err, ErrNotOwner) {
		t.Errorf("Grants: %v", err)
	}
	if err := b.Revoke("g1"); !errors.Is(err, ErrNotOwner) {
		t.Errorf("Revoke: %v", err)
	}
	if _, err := b.Request(1); !errors.Is(err, ErrNotOwner) {
		t.Errorf("Request: %v", err)
	}
	if _, err := b.Aliases(); !errors.Is(err, ErrNotOwner) {
		t.Errorf("Aliases: %v", err)
	}
	if err := b.Reload(); !errors.Is(err, ErrNotOwner) {
		t.Errorf("Reload: %v", err)
	}
}

func TestDedupe(t *testing.T) {
	f := newFixture(t)
	id := mustPending(t, f.decide(aliasReq("build")))
	o, err := f.b.File(Ask{Request: aliasReq("build"), Context: "second text"})
	if err != nil {
		t.Fatal(err)
	}
	if o.Request != id {
		t.Fatalf("a sessionless File not merged into the sessionless Decide: %+v", o)
	}
	again := mustPending(t, f.decide(aliasReq("build")))
	if again != id {
		t.Fatalf("same request, new number: %d then %d", id, again)
	}

	a1, _ := f.b.File(Ask{Request: aliasReq("build"), Session: "a", Context: "first"})
	a2, _ := f.b.File(Ask{Request: aliasReq("build"), Session: "a", Context: "changed"})
	b1, _ := f.b.File(Ask{Request: aliasReq("build"), Session: "b", Context: "other"})
	if a1.Request != a2.Request || a1.Request == b1.Request || a1.Request == id {
		t.Fatalf("sessions: a %d %d, b %d, none %d", a1.Request, a2.Request, b1.Request, id)
	}
	ri, err := f.b.Request(a1.Request)
	if err != nil {
		t.Fatal(err)
	}
	if ri.Attempts != 2 || ri.Ask == nil || ri.Ask.Context != "first" || ri.Session != "a" {
		t.Fatalf("merged request: %+v %+v", ri, ri.Ask)
	}
	if ri, _ := f.b.Request(id); ri.Attempts != 3 || ri.Ask != nil {
		t.Fatalf("attempts %d", ri.Attempts)
	}
}

// A repeat of a pending request rewrites requests.json at most once an
// AttemptsEvery; the repeats in between are counted all the same, with
// the next write, and a write that fails loses none of them.
func TestDedupeWritesBounded(t *testing.T) {
	f := newFixture(t)
	id := mustPending(t, f.decide(aliasReq("build")))
	writes := 0
	writeFault = func(name string) error {
		if name == requestsFile {
			writes++
		}
		return nil
	}
	t.Cleanup(func() { writeFault = nil })
	for range 50 {
		if got := mustPending(t, f.decide(aliasReq("build"))); got != id {
			t.Fatalf("request %d, want %d", got, id)
		}
	}
	if writes != 1 {
		t.Fatalf("%d writes of %s for 50 repeats", writes, requestsFile)
	}
	if ri, _ := f.b.Request(id); ri.Attempts != 2 {
		t.Fatalf("attempts on file %d, want 2", ri.Attempts)
	}
	// A write that fails keeps the count held.
	writeFault = func(name string) error {
		if name == requestsFile {
			return errors.New("disk full")
		}
		return nil
	}
	f.clock.Add(AttemptsEvery)
	_ = f.decide(aliasReq("build")) // denied: the host cannot write it

	writeFault = nil
	f.clock.Add(AttemptsEvery)
	mustPending(t, f.decide(aliasReq("build")))
	if ri, _ := f.b.Request(id); ri.Attempts != 53 {
		t.Fatalf("attempts %d, want 53", ri.Attempts)
	}
	// Repeats count nothing toward the new requests a minute, which
	// still count from the requests' times.
	f.b.maxPerMinute = 3 // the first, filed 20s ago, is one
	mustPending(t, f.decide(egressReq("db1.corp.example", "10.0.0.5")))
	mustPending(t, f.decide(egressReq("db2.corp.example", "10.0.0.6")))
	if o := f.decide(egressReq("db3.corp.example", "10.0.0.7")); o.Verdict != Denied {
		t.Fatalf("a third new request in a minute filed: %+v", o)
	}
}

func TestLimitPending(t *testing.T) {
	f := newFixture(t)
	f.src.set(func(c *config.Config) { c.Trust = config.TrustHostile })
	if err := f.b.Reload(); err != nil {
		t.Fatal(err)
	}
	var first uint64
	for i := range MaxPending {
		f.clock.Add(10 * time.Second)
		id := mustPending(t, f.decide(egressReq(fmt.Sprintf("h%d.example.com", i), "93.184.216.34")))
		if i == 0 {
			first = id
		}
	}
	f.clock.Add(10 * time.Second)
	before := f.read(requestsFile)
	o := f.decide(egressReq("one-more.example.com", "93.184.216.34"))
	if o.Verdict != Denied || !strings.Contains(o.Reason, "already pending") {
		t.Fatalf("101st: %+v", o)
	}
	if !bytes.Equal(before, f.read(requestsFile)) {
		t.Fatal("the refused request changed requests.json")
	}
	// A request already pending is still found at the limit.
	if got := mustPending(t, f.decide(egressReq("h0.example.com", "93.184.216.34"))); got != first {
		t.Fatalf("dedupe at the limit: %d, want %d", got, first)
	}
	list, _, _ := f.b.List(Query{})
	if len(list) != MaxPending {
		t.Fatalf("%d pending", len(list))
	}
}

func TestLimitPerMinute(t *testing.T) {
	f := newFixture(t)
	f.src.set(func(c *config.Config) { c.Trust = config.TrustHostile })
	if err := f.b.Reload(); err != nil {
		t.Fatal(err)
	}
	var ids []uint64
	for i := range MaxNewPerMinute {
		f.clock.Add(time.Second)
		ids = append(ids, mustPending(t, f.decide(egressReq(fmt.Sprintf("h%d.example.com", i), "93.184.216.34"))))
	}
	// Dedupe hits do not count, and are still answered.
	for range 5 {
		if got := mustPending(t, f.decide(egressReq("h0.example.com", "93.184.216.34"))); got != ids[0] {
			t.Fatalf("dedupe: %d", got)
		}
	}
	before := f.read(requestsFile)
	o := f.decide(egressReq("eleventh.example.com", "93.184.216.34"))
	if o.Verdict != Denied || !strings.Contains(o.Reason, "last minute") {
		t.Fatalf("11th: %+v", o)
	}
	if !bytes.Equal(before, f.read(requestsFile)) {
		t.Fatal("the refused request changed requests.json")
	}
	f.clock.Add(time.Minute)
	if got := mustPending(t, f.decide(egressReq("eleventh.example.com", "93.184.216.34"))); got != ids[len(ids)-1]+1 {
		t.Fatalf("after a minute: #%d", got)
	}
}

func TestExpiry(t *testing.T) {
	f := newFixture(t)
	id := mustPending(t, f.decide(aliasReq("build")))
	f.clock.Add(RequestTTL - time.Second)
	if ri, _ := f.b.Request(id); ri.State != StatePending {
		t.Fatalf("expired early: %+v", ri)
	}
	since := f.b.st.reqs.Seq
	f.clock.Add(time.Second)
	list, cursor, err := f.b.List(Query{All: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].State != StateExpired || list[0].DecidedBy != ByExpiry || cursor <= since {
		t.Fatalf("after 24h: %+v cursor %d", list, cursor)
	}
	if _, err := f.b.Answer(id, Reply{Verdict: ReplyAllow, Scope: ScopeHours, Hours: 1, By: ByCLI}); !errors.Is(err, ErrAlreadyAnswered) || !strings.Contains(err.Error(), "expiry") {
		t.Fatalf("answering an expired request: %v", err)
	}
	// The same crossing now files a new request.
	if got := mustPending(t, f.decide(aliasReq("build"))); got == id {
		t.Fatal("an expired request was reused")
	}

	// The ticker's path expires too, without a List.
	id2 := mustPending(t, f.decide(egressReq("db.corp.example", "10.0.0.5")))
	f.clock.Add(RequestTTL)
	f.b.expire()
	f.b.mu.Lock()
	st := f.b.st.reqs.find(id2).State
	f.b.mu.Unlock()
	if st != StateExpired {
		t.Fatalf("expire: %s", st)
	}
}

func TestFirstAnswerWins(t *testing.T) {
	f := newFixture(t)
	id := mustPending(t, f.decide(urlReq("https://example.com/page")))
	if len(f.dialog.ids) != 1 || f.dialog.ids[0] != id {
		t.Fatalf("dialog asked %v", f.dialog.ids)
	}
	// Merged into the pending one: no second dialog.
	mustPending(t, f.decide(urlReq("https://example.com/page")))
	if len(f.dialog.ids) != 1 {
		t.Fatalf("dialog asked again: %v", f.dialog.ids)
	}
	f.answer(id, Reply{Verdict: ReplyDeny, By: ByCLI})
	_, err := f.b.Answer(id, Reply{Verdict: ReplyAllow, Scope: ScopeOnce, By: ByDialog})
	if !errors.Is(err, ErrAlreadyAnswered) || !strings.Contains(err.Error(), "already answered by cli") {
		t.Fatalf("dialog after cli: %v", err)
	}
	if len(f.open.opened()) != 0 {
		t.Fatal("the dropped dialog answer opened the URL")
	}
	if ri, _ := f.b.Request(id); ri.State != StateDenied || ri.DecidedBy != ByCLI {
		t.Fatalf("%+v", ri)
	}

	// Allowing a URL request opens the stored URL, once.
	id2 := mustPending(t, f.decide(urlReq("https://other.example/x?y=1")))
	f.answer(id2, Reply{Verdict: ReplyAllow, Scope: ScopeOnce, By: ByDialog})
	if got := f.open.opened(); len(got) != 1 || got[0] != "https://other.example/x?y=1" {
		t.Fatalf("opened %v", got)
	}
	if g, _ := f.b.Grants(); len(g) != 0 {
		t.Fatalf("a once URL answer granted %+v", g)
	}
	if _, err := f.b.Answer(999, Reply{Verdict: ReplyDeny, By: ByCLI}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
}

func TestNilOpenerRefusesURLAllow(t *testing.T) {
	f := newFixture(t)
	o := f.options()
	o.Opener = nil
	f.b.Close()
	f.b = nil
	b, err := Open(o)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	out, _ := b.Decide(urlReq("https://example.com/"))
	if _, err := b.Answer(out.Request, Reply{Verdict: ReplyAllow, Scope: ScopeOnce, By: ByCLI}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("%v", err)
	}
	if ri, _ := b.Request(out.Request); ri.State != StatePending {
		t.Fatalf("%+v", ri)
	}
}

// A once grant allows exactly one crossing, however many race for it.
func TestOnceGrantConsumedOnce(t *testing.T) {
	f := newFixture(t)
	id := mustPending(t, f.decide(aliasReq("build")))
	res := f.answer(id, Reply{Verdict: ReplyAllow, Scope: ScopeOnce})
	if res.Grant == "" {
		t.Fatalf("no grant: %+v", res)
	}
	second := f.openBroker() // another Broker on the same files
	var (
		mu      sync.Mutex
		allowed int
		wg      sync.WaitGroup
	)
	for i := range 40 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b := f.b
			if i%2 == 1 {
				b = second
			}
			o, err := b.Decide(aliasReq("build"))
			if err != nil {
				t.Error(err)
			}
			if o.Verdict == Allowed {
				mu.Lock()
				allowed++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if allowed != 1 {
		t.Fatalf("a once grant allowed %d crossings", allowed)
	}
	if g, _ := f.b.Grants(); len(g) != 0 {
		t.Fatalf("grant left: %+v", g)
	}
}

func TestOnceGrantExpires(t *testing.T) {
	f := newFixture(t)
	id := mustPending(t, f.decide(aliasReq("build")))
	f.answer(id, Reply{Verdict: ReplyAllow, Scope: ScopeOnce})
	f.clock.Add(OnceTTL)
	if o := f.decide(aliasReq("build")); o.Verdict != Pending {
		t.Fatalf("an expired once grant: %+v", o)
	}
}

func TestPersistence(t *testing.T) {
	f := newFixture(t)
	id1 := mustPending(t, f.decide(aliasReq("build")))
	id2 := mustPending(t, f.decide(egressReq("db.corp.example", "10.0.0.5")))
	res := f.answer(id1, Reply{Verdict: ReplyAllow, Scope: ScopeHours, Hours: 8})
	f.reopen()
	if o := f.decide(aliasReq("build")); o.Verdict != Allowed {
		t.Fatalf("grant lost across a reopen: %+v", o)
	}
	g, _ := f.b.Grants()
	if len(g) != 1 || g[0].ID != res.Grant || g[0].Left != 8*time.Hour {
		t.Fatalf("grants %+v", g)
	}
	if ri, _ := f.b.Request(id2); ri.State != StatePending {
		t.Fatalf("pending lost: %+v", ri)
	}
	id3 := mustPending(t, f.decide(urlReq("https://example.com/")))
	if id3 != id2+1 {
		t.Fatalf("numbers restarted: #%d after #%d", id3, id2)
	}
	_, cursor, _ := f.b.List(Query{})
	if cursor == 0 {
		t.Fatal("seq lost")
	}
	info, err := os.Stat(filepath.Join(f.dir, grantsFile))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("grants.json mode: %v %v", info, err)
	}
	// Revoked, it no longer allows.
	if err := f.b.Revoke(res.Grant); err != nil {
		t.Fatal(err)
	}
	if o := f.decide(aliasReq("build")); o.Verdict != Pending {
		t.Fatalf("after revoke: %+v", o)
	}
	if err := f.b.Revoke(res.Grant); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoke twice: %v", err)
	}
}

// A file that cannot be used counts as empty, and is never rewritten.
func TestUnusableFilesNotRewritten(t *testing.T) {
	future := f2s(f2Time().Add(time.Hour))
	cases := map[string]string{
		"corrupt": "{not json",
		"newer":   `{"version": 2, "next_id": 5, "grants": [{"id": "g1", "kind": "alias", "key": "alias:build", "expires": "` + future + `"}], "requests": []}`,
		"trailer": `{"version": 1, "next_id": 1, "grants": []} {}`,
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.b.Close()
			for _, file := range []string{grantsFile, requestsFile} {
				if err := os.WriteFile(filepath.Join(f.dir, file), []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			f.b = f.openBroker()
			o := f.decide(aliasReq("build"))
			if o.Verdict != Denied || !strings.Contains(o.Reason, requestsFile) {
				t.Fatalf("ask with requests.json unusable: %+v", o)
			}
			if _, err := f.b.Answer(1, Reply{Verdict: ReplyDeny, By: ByCLI}); !errors.Is(err, ErrUnavailable) {
				t.Fatalf("answer: %v", err)
			}
			if _, err := f.b.Grants(); !errors.Is(err, ErrUnavailable) {
				t.Fatalf("grants: %v", err)
			}
			for _, file := range []string{grantsFile, requestsFile} {
				if got := f.read(file); string(got) != content {
					t.Fatalf("%s rewritten: %q", file, got)
				}
			}
		})
	}

	// requests.json fine, grants.json newer: the grant it holds never
	// applies, an answer that would grant fails, and it is not rewritten.
	f := newFixture(t)
	f.b.Close()
	newer := cases["newer"]
	if err := os.WriteFile(filepath.Join(f.dir, grantsFile), []byte(newer), 0o600); err != nil {
		t.Fatal(err)
	}
	f.b = f.openBroker()
	id := mustPending(t, f.decide(aliasReq("build")))
	if _, err := f.b.Answer(id, Reply{Verdict: ReplyAllow, Scope: ScopeHours, Hours: 1, By: ByCLI}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("answer: %v", err)
	}
	if ri, _ := f.b.Request(id); ri.State != StatePending {
		t.Fatalf("recorded without its grant: %+v", ri)
	}
	if got := f.read(grantsFile); string(got) != newer {
		t.Fatalf("grants.json rewritten: %q", got)
	}
	// A deny needs no grant, and is recorded.
	f.answer(id, Reply{Verdict: ReplyDeny})
}

func f2Time() time.Time      { return newClock().Now() }
func f2s(t time.Time) string { return t.Format(time.RFC3339) }

// Symlinks planted at the state files are not followed: a read treats the
// file as unusable, nothing is written through them, and nothing outside
// the env dir changes.
func TestSymlinksNotFollowed(t *testing.T) {
	f := newFixture(t)
	f.b.Close()
	outside := t.TempDir()
	future := f2s(f2Time().Add(time.Hour))
	grants := `{"version": 1, "next_id": 2, "grants": [{"id": "g1", "kind": "alias", "key": "alias:build", "expires": "` + future + `"}]}`
	reqs := `{"version": 1, "next_id": 1, "seq": 0, "requests": []}`
	og, or := filepath.Join(outside, "g.json"), filepath.Join(outside, "r.json")
	if err := os.WriteFile(og, []byte(grants), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(or, []byte(reqs), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(og, filepath.Join(f.dir, grantsFile)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(or, filepath.Join(f.dir, requestsFile)); err != nil {
		t.Fatal(err)
	}
	f.b = f.openBroker()
	o := f.decide(aliasReq("build"))
	if o.Verdict == Allowed {
		t.Fatal("a grant read through a symlink applied")
	}
	if o.Verdict != Denied {
		t.Fatalf("filed through a symlinked requests.json: %+v", o)
	}
	if o, _ := f.b.File(Ask{Request: aliasReq("build"), Context: "x"}); o.Verdict != Denied {
		t.Fatalf("File: %+v", o)
	}
	if err := f.b.Revoke("g1"); err == nil {
		t.Fatal("revoked through a symlink")
	}
	if g, _ := os.ReadFile(og); string(g) != grants {
		t.Fatalf("outside grants changed: %s", g)
	}
	if r, _ := os.ReadFile(or); string(r) != reqs {
		t.Fatalf("outside requests changed: %s", r)
	}
	for _, name := range []string{grantsFile, requestsFile} {
		if fi, err := os.Lstat(filepath.Join(f.dir, name)); err != nil || fi.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("%s: %v %v", name, fi, err)
		}
	}

	// The writer itself replaces a link, never writes through it.
	if err := f.b.writeDoc(grantsFile, &grantsDoc{Version: 1}); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(filepath.Join(f.dir, grantsFile)); err != nil || !fi.Mode().IsRegular() {
		t.Fatalf("not replaced: %v %v", fi, err)
	}
	if g, _ := os.ReadFile(og); string(g) != grants {
		t.Fatalf("written through: %s", g)
	}
}

// A planted symlink at decisions.log turns logging off rather than
// appending through it.
func TestDecisionLogSymlink(t *testing.T) {
	f := newFixture(t)
	f.b.Close()
	target := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.WriteFile(target, []byte("keep\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(f.dir, decisionsFile)
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, p); err != nil {
		t.Fatal(err)
	}
	f.b = f.openBroker()
	f.decide(aliasReq("build"))
	if got, _ := os.ReadFile(target); string(got) != "keep\n" {
		t.Fatalf("logged through a symlink: %q", got)
	}
}

func TestWaitCapPerLink(t *testing.T) {
	f := newFixture(t)
	l := f.b.Link()
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	started := make(chan struct{}, MaxWaitsPerLink)
	for range MaxWaitsPerLink {
		wg.Add(1)
		go func() {
			defer wg.Done()
			started <- struct{}{}
			if _, err := l.Wait(ctx, 1<<62); err != nil {
				t.Error(err)
			}
		}()
	}
	for range MaxWaitsPerLink {
		<-started
	}
	deadline := time.Now().Add(5 * time.Second)
	for l.waits.Load() < MaxWaitsPerLink && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	done := make(chan error, 1)
	go func() {
		_, err := l.Wait(context.Background(), 1<<62)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrTooManyWaits) || err.Error() == "" || !strings.Contains(err.Error(), "too many waits") {
			t.Fatalf("17th: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the 17th wait blocked")
	}
	// Another link has its own allowance.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel2()
	if _, err := f.b.Link().Wait(ctx2, 1<<62); err != nil {
		t.Fatalf("another link: %v", err)
	}
	cancel()
	wg.Wait()
	ctx3, cancel3 := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel3()
	if _, err := l.Wait(ctx3, 1<<62); err != nil {
		t.Fatalf("after the waits ended: %v", err)
	}
}

func TestWaitWakesOnDecision(t *testing.T) {
	f := newFixture(t)
	id := mustPending(t, f.decide(aliasReq("build")))
	_, cursor, _ := f.b.List(Query{})
	got := make(chan uint64, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		seq, _ := f.b.Link().Wait(ctx, cursor)
		got <- seq
	}()
	time.Sleep(10 * time.Millisecond)
	f.answer(id, Reply{Verdict: ReplyDeny})
	seq := <-got
	if seq <= cursor {
		t.Fatalf("woke with %d, cursor %d", seq, cursor)
	}
	list, _, _ := f.b.List(Query{Since: cursor})
	if len(list) != 1 || list[0].ID != id || list[0].State != StateDenied {
		t.Fatalf("since: %+v", list)
	}
	if list, _, _ := f.b.List(Query{}); len(list) != 0 {
		t.Fatalf("pending: %+v", list)
	}
}

// alwaysOffer is request id's "always" offer.
func (f *fixture) alwaysOffer(id uint64) Offer {
	f.t.Helper()
	_, offers, withheld, err := f.b.RequestOffers(id)
	if err != nil {
		f.t.Fatalf("offers of #%d: %v", id, err)
	}
	for _, o := range offers {
		if o.Scope == ScopeAlways {
			return o
		}
	}
	f.t.Fatalf("#%d has no always offer: %+v, withheld %+v", id, offers, withheld)
	return Offer{}
}

// An "always" answer is offered with its diff, is refused without the
// diff's hash or with a stale one, writing nothing, and once written, the
// very next crossing is allowed.
func TestAlwaysAllowsNextRequest(t *testing.T) {
	f := newFixture(t)
	id := mustPending(t, f.decide(aliasReq("build")))
	o := f.alwaysOffer(id)
	if o.Diff == "" || o.DiffHash != DiffHash(o.Diff) || !strings.Contains(o.Diff, "alias.build.policy = allow") || !strings.Contains(o.Effect, `policy = "allow"`) {
		t.Fatalf("offer: %+v", o)
	}
	if _, err := f.b.Answer(id, Reply{Verdict: ReplyAllow, Scope: ScopeAlways, By: ByCLI}); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("without a confirmation: %v", err)
	}
	if _, writes := f.w.calls(); writes != 0 {
		t.Fatal("written before confirming")
	}
	if ri, _ := f.b.Request(id); ri.State != StatePending {
		t.Fatalf("%+v", ri)
	}
	if _, err := f.b.Answer(id, Reply{Verdict: ReplyAllow, Scope: ScopeAlways, By: ByCLI, Confirm: "deadbeef"}); !errors.Is(err, ErrStaleDiff) {
		t.Fatalf("wrong hash: %v", err)
	}
	// The file changed between showing and confirming.
	f.w.mu.Lock()
	f.w.drift = "# someone else's edit\n"
	f.w.mu.Unlock()
	if _, err := f.b.Answer(id, Reply{Verdict: ReplyAllow, Scope: ScopeAlways, By: ByCLI, Confirm: o.DiffHash}); !errors.Is(err, ErrStaleDiff) {
		t.Fatalf("stale diff: %v", err)
	}
	if _, writes := f.w.calls(); writes != 0 {
		t.Fatal("a stale diff was written")
	}
	if ri, _ := f.b.Request(id); ri.State != StatePending {
		t.Fatalf("after a stale diff: %+v", ri)
	}
	o = f.alwaysOffer(id)
	if !strings.Contains(o.Diff, "someone else's edit") {
		t.Fatalf("the offer did not follow the file: %q", o.Diff)
	}
	res := f.answer(id, Reply{Verdict: ReplyAllow, Scope: ScopeAlways, Confirm: o.DiffHash, By: ByWeb})
	if !res.Recorded || res.Request.State != StateAllowed || res.Request.DecidedBy != ByWeb || res.Diff != o.Diff || res.DiffHash != o.DiffHash || !strings.Contains(res.Effect, `policy = "allow"`) {
		t.Fatalf("confirmed: %+v", res)
	}
	if _, writes := f.w.calls(); writes != 1 {
		t.Fatalf("writes: %d", writes)
	}
	if o := f.decide(aliasReq("build")); o.Verdict != Allowed {
		t.Fatalf("the next crossing after always: %+v", o)
	}
}

func TestAlwaysEgressAndURL(t *testing.T) {
	f := newFixture(t)
	id := mustPending(t, f.decide(egressReq("db.corp.example", "10.0.0.5")))
	o := f.alwaysOffer(id)
	if !strings.Contains(o.Diff, "vm.default.egress_allow = [db.corp.example]") {
		t.Fatalf("egress diff: %q", o.Diff)
	}
	f.answer(id, Reply{Verdict: ReplyAllow, Scope: ScopeAlways, Confirm: o.DiffHash})
	if o := f.decide(egressReq("db.corp.example", "10.0.0.5")); o.Verdict != Allowed {
		t.Fatalf("after always: %+v", o)
	}
	// A name written into config.toml reaches whatever it resolves to.
	if o := f.decide(egressReq("db.corp.example", "10.0.0.6")); o.Verdict != Allowed {
		t.Fatalf("another private address after always: %+v", o)
	}

	uid := mustPending(t, f.decide(urlReq("https://example.com/a")))
	f.answer(uid, Reply{Verdict: ReplyAllow, Scope: ScopeAlways, Confirm: f.alwaysOffer(uid).DiffHash})
	if o := f.decide(urlReq("https://example.com/b")); o.Verdict != Allowed {
		t.Fatalf("url after always: %+v", o)
	}

	// Under hostile, a URL is never allowed always: not offered, and an
	// answer that tries is refused.
	f.src.set(func(c *config.Config) { c.Trust = config.TrustHostile })
	if err := f.b.Reload(); err != nil {
		t.Fatal(err)
	}
	hid := mustPending(t, f.decide(urlReq("https://hostile.example/")))
	_, offers, withheld, err := f.b.RequestOffers(hid)
	if err != nil {
		t.Fatal(err)
	}
	if len(offers) != 2 || offers[0].Scope != ScopeOnce || offers[1].Scope != ScopeHours ||
		len(withheld) != 1 || withheld[0].Scope != ScopeAlways || !strings.Contains(withheld[0].Why, "hostile") {
		t.Fatalf("hostile url offers %+v, withheld %+v", offers, withheld)
	}
	if _, err := f.b.Answer(hid, Reply{Verdict: ReplyAllow, Scope: ScopeAlways, By: ByCLI, Confirm: DiffHash("")}); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("hostile always url: %v", err)
	}
}

// What each request is offered: once, hours with its bounds and the hours
// left to the person, always with the diff; nothing once answered.
func TestOffers(t *testing.T) {
	f := newFixture(t)
	id := mustPending(t, f.decide(egressReq("db.corp.example", "10.0.0.5")))
	ri, offers, withheld, err := f.b.RequestOffers(id)
	if err != nil || ri.ID != id || len(withheld) != 0 || len(offers) != 3 {
		t.Fatalf("%+v %+v %+v %v", ri, offers, withheld, err)
	}
	once, hours, always := offers[0], offers[1], offers[2]
	if once.Scope != ScopeOnce || once.Effect != "allows that once" || once.Diff != "" || once.DiffHash != "" {
		t.Fatalf("once: %+v", once)
	}
	if hours.Scope != ScopeHours || hours.HoursMin != 1 || hours.HoursMax != MaxHours ||
		!strings.HasSuffix(hours.Effect, " for "+HoursPlaceholder+" hours") || !strings.Contains(hours.Effect, "db.corp.example") || hours.Diff != "" {
		t.Fatalf("hours: %+v", hours)
	}
	if always.Scope != ScopeAlways || always.DiffHash != DiffHash(always.Diff) || !strings.Contains(always.Effect, "egress_allow") {
		t.Fatalf("always: %+v", always)
	}
	if _, writes := f.w.calls(); writes != 0 {
		t.Fatal("offering wrote")
	}
	f.answer(id, Reply{Verdict: ReplyDeny, By: ByWeb})
	ri, offers, withheld, err = f.b.RequestOffers(id)
	if err != nil || ri.DecidedBy != ByWeb || offers != nil || withheld != nil {
		t.Fatalf("answered: %+v %+v %+v %v", ri, offers, withheld, err)
	}
	if _, _, _, err := f.b.RequestOffers(9999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown: %v", err)
	}
}

// An "always" diff too long to show is not offered, and not answered.
func TestAlwaysTooLongIsNotOffered(t *testing.T) {
	f := newFixture(t)
	id := mustPending(t, f.decide(aliasReq("build")))
	f.w.mu.Lock()
	f.w.drift = strings.Repeat("+x\n", MaxOfferDiffLines)
	f.w.mu.Unlock()
	_, offers, withheld, err := f.b.RequestOffers(id)
	if err != nil || len(offers) != 2 || len(withheld) != 1 || withheld[0].Scope != ScopeAlways {
		t.Fatalf("%+v %+v %v", offers, withheld, err)
	}
	diff, _ := f.w.Diff(config.Edit{Set: map[string]any{"alias.build.policy": config.AliasAllow}})
	if _, err := f.b.Answer(id, Reply{Verdict: ReplyAllow, Scope: ScopeAlways, By: ByCLI, Confirm: DiffHash(diff)}); err == nil {
		t.Fatal("answered always with a diff too long to show")
	}
	if _, writes := f.w.calls(); writes != 0 {
		t.Fatal("written")
	}
}

// Who answered is recorded as the answer says it, from the kinds a
// daemon client may be; any other is refused.
func TestAnswerBy(t *testing.T) {
	f := newFixture(t)
	for _, by := range []string{ByCLI, ByWeb, ByDialog} {
		id := mustPending(t, f.decide(aliasReq("build")))
		if res := f.answer(id, Reply{Verdict: ReplyDeny, By: by}); res.Request.DecidedBy != by {
			t.Fatalf("%s: %+v", by, res.Request)
		}
	}
	id := mustPending(t, f.decide(aliasReq("build")))
	for _, by := range []string{"daemon", "expiry", "", "CLI"} {
		if _, err := f.b.Answer(id, Reply{Verdict: ReplyDeny, By: by}); !errors.Is(err, ErrBadRequest) {
			t.Fatalf("%q: %v", by, err)
		}
	}
}

// A grant made by name for a private address does not lift the ask for
// another private address the name later resolves to.
func TestEgressGrantRebinding(t *testing.T) {
	f := newFixture(t)
	id := mustPending(t, f.decide(egressReq("db.corp.example", "10.0.0.5")))
	res := f.answer(id, Reply{Verdict: ReplyAllow, Scope: ScopeHours, Hours: 2})
	g, _ := f.b.Grants()
	if len(g) != 1 || len(g[0].Addrs) != 1 || g[0].Addrs[0] != "10.0.0.5" || g[0].ID != res.Grant {
		t.Fatalf("grant %+v", g)
	}
	if o := f.decide(egressReq("db.corp.example", "10.0.0.5")); o.Verdict != Allowed {
		t.Fatalf("granted address: %+v", o)
	}
	if o := f.decide(egressReq("db.corp.example", "10.0.0.6")); o.Verdict != Pending {
		t.Fatalf("rebound address: %+v", o)
	}
}

// Hours and always answers decide the other pending requests of the same
// kind and key; once and deny decide only the one answered.
func TestCascade(t *testing.T) {
	f := newFixture(t)
	a, _ := f.b.File(Ask{Request: aliasReq("build"), Session: "a"})
	b, _ := f.b.File(Ask{Request: aliasReq("build"), Session: "b"})
	c, _ := f.b.File(Ask{Request: aliasReq("build"), Session: "c"})
	other := mustPending(t, f.decide(egressReq("db.corp.example", "10.0.0.5")))
	f.answer(a.Request, Reply{Verdict: ReplyAllow, Scope: ScopeOnce})
	if o := f.decide(aliasReq("build")); o.Verdict != Allowed {
		t.Fatalf("the once grant: %+v", o)
	}
	if ri, _ := f.b.Request(b.Request); ri.State != StatePending {
		t.Fatalf("once cascaded: %+v", ri)
	}
	f.answer(b.Request, Reply{Verdict: ReplyDeny})
	if ri, _ := f.b.Request(c.Request); ri.State != StatePending {
		t.Fatalf("deny cascaded: %+v", ri)
	}
	d, _ := f.b.File(Ask{Request: aliasReq("build"), Session: "d"})
	res := f.answer(c.Request, Reply{Verdict: ReplyAllow, Scope: ScopeHours, Hours: 1})
	if len(res.Also) != 1 || res.Also[0] != d.Request {
		t.Fatalf("also %v", res.Also)
	}
	ri, _ := f.b.Request(d.Request)
	if ri.State != StateAllowed || ri.Reason != fmt.Sprintf("allowed with #%d", c.Request) || ri.DecidedBy != ByCLI {
		t.Fatalf("cascaded: %+v", ri)
	}
	if ri, _ := f.b.Request(other); ri.State != StatePending {
		t.Fatalf("another key cascaded: %+v", ri)
	}
	// An egress request at another private address is not decided by a
	// grant that does not hold it.
	e1, _ := f.b.File(Ask{Request: egressReq("db.corp.example", "10.0.0.6"), Session: "x"})
	f.answer(other, Reply{Verdict: ReplyAllow, Scope: ScopeHours, Hours: 1})
	if ri, _ := f.b.Request(e1.Request); ri.State != StatePending {
		t.Fatalf("cascaded past its addresses: %+v", ri)
	}
}

func TestAnswerChecks(t *testing.T) {
	f := newFixture(t)
	id := mustPending(t, f.decide(aliasReq("build")))
	for _, rp := range []Reply{
		{Verdict: "maybe", By: ByCLI},
		{Verdict: ReplyAllow, Scope: "forever", By: ByCLI},
		{Verdict: ReplyAllow, Scope: ScopeHours, Hours: 0, By: ByCLI},
		{Verdict: ReplyAllow, Scope: ScopeHours, Hours: MaxHours + 1, By: ByCLI},
		{Verdict: ReplyAllow, Scope: ScopeOnce, By: "sandbox"},
	} {
		if _, err := f.b.Answer(id, rp); !errors.Is(err, ErrBadRequest) {
			t.Errorf("%+v: %v", rp, err)
		}
	}
	res := f.answer(id, Reply{Verdict: ReplyAllow, Scope: ScopeHours, Hours: MaxHours})
	if g, _ := f.b.Grants(); len(g) != 1 || g[0].Left != MaxHours*time.Hour || res.Effect == "" {
		t.Fatalf("%+v %+v", g, res)
	}
}

func TestFileText(t *testing.T) {
	f := newFixture(t)
	for name, a := range map[string]Ask{
		"bidi":         {Context: "fine \u202e evil"},
		"escape":       {Context: "\x1b[31m red"},
		"long context": {Context: strings.Repeat("x", MaxContext+1)},
		"long multi":   {Context: strings.Repeat("é", MaxContext/2+1)},
		"many options": {Options: []string{"a", "b", "c", "d", "e", "f"}},
		"long option":  {Options: []string{strings.Repeat("o", MaxOption+1)}},
		"newline opt":  {Options: []string{"a\nb"}},
		"long rec":     {Recommendation: strings.Repeat("r", MaxRecommendation+1)},
		"long mean":    {Meanwhile: strings.Repeat("m", MaxMeanwhile+1)},
		"long session": {Session: strings.Repeat("s", MaxSession+1)},
		"session nl":   {Session: "a\nb"},
	} {
		a.Request = aliasReq("build")
		o, err := f.b.File(a)
		if err != nil || o.Verdict != Denied || o.Request != 0 {
			t.Errorf("%s: %+v %v", name, o, err)
		}
	}
	if list, _, _ := f.b.List(Query{All: true}); len(list) != 0 {
		t.Fatalf("filed: %+v", list)
	}
	o, _ := f.b.File(Ask{Request: aliasReq("build"), Context: "line one\nline two\ttab", Options: []string{"wait", "skip"},
		Recommendation: "wait", Meanwhile: "I will do the docs", Session: "s1"})
	ri, _ := f.b.Request(mustPending(t, o))
	if ri.Ask == nil || ri.Ask.Context != `line one\u000Aline two\u0009tab` || len(ri.Ask.Options) != 2 || ri.Effect == "" || strings.Contains(ri.Effect, "docs") {
		t.Fatalf("%+v %+v", ri, ri.Ask)
	}
}

// The session the sandbox claims never changes a verdict.
func TestSessionNeverChangesVerdict(t *testing.T) {
	f := newFixture(t)
	f.b.maxPerMinute = 1000
	reqs := []policy.Request{
		aliasReq("build"), aliasReq("free"), aliasReq("nope"),
		egressReq("db.corp.example", "10.0.0.5"), egressReq("api.example.com", "93.184.216.34"),
		egressReq("localhost", "127.0.0.1"), urlReq("https://example.com/"),
		{Kind: policy.KindHostExec}, {Kind: "weird"},
	}
	for _, r := range reqs {
		base, _ := f.b.File(Ask{Request: r})
		for _, s := range []string{"a", "root", "cli", strings.Repeat("z", MaxSession)} {
			o, _ := f.b.File(Ask{Request: r, Session: s})
			if o.Verdict != base.Verdict {
				t.Errorf("%+v: session %q: %s, without: %s", r, s, o.Verdict, base.Verdict)
			}
		}
	}
}

func TestWritableError(t *testing.T) {
	f := newFixture(t)
	o := f.options()
	o.Writable = func() ([]string, error) { return nil, errors.New("no mounts") }
	f.b.Close()
	b, err := Open(o)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if out, _ := b.Decide(aliasReq("free")); out.Verdict != Denied {
		t.Fatalf("alias with an unknown writable set: %+v", out)
	}
}

func TestEgressOffUnlessVM(t *testing.T) {
	f := newFixture(t)
	if o := f.decide(egressReq("api.example.com", "93.184.216.34")); o.Verdict != Allowed {
		t.Fatalf("vm, egress on: %+v", o)
	}
	for name, change := range map[string]func(c *config.Config){
		"container":  func(c *config.Config) { c.Isolation, c.Profile = config.KindContainer, "" },
		"egress off": func(c *config.Config) { c.Egress = false },
		"bad ports":  func(c *config.Config) { c.EgressPorts = "nope" },
		"bad allow":  func(c *config.Config) { c.EgressAllow = []string{"a b"} },
		"bad deny":   func(c *config.Config) { c.EgressDeny = []string{"none"} },
	} {
		f.src.set(func(c *config.Config) { *c = baseConfig(); change(c) })
		if err := f.b.Reload(); err != nil {
			t.Fatal(err)
		}
		if o := f.decide(egressReq("api.example.com", "93.184.216.34")); o.Verdict != Denied {
			t.Errorf("%s: %+v", name, o)
		}
	}
	// A Source that fails keeps the policy as it was.
	f.src.set(func(c *config.Config) { *c = baseConfig() })
	if err := f.b.Reload(); err != nil {
		t.Fatal(err)
	}
	f.src.mu.Lock()
	f.src.err = errors.New("config.toml is broken")
	f.src.mu.Unlock()
	if err := f.b.Reload(); err == nil {
		t.Fatal("no error")
	}
	if o := f.decide(egressReq("api.example.com", "93.184.216.34")); o.Verdict != Allowed {
		t.Fatalf("policy lost on a failed reload: %+v", o)
	}
}

func TestDecisionsLog(t *testing.T) {
	f := newFixture(t)
	for range 50 {
		f.decide(egressReq("api.example.com", "93.184.216.34"))
	}
	id := mustPending(t, f.decide(aliasReq("build")))
	f.answer(id, Reply{Verdict: ReplyDeny})
	data := string(f.read(decisionsFile))
	if strings.Count(data, "\n") != 2 || !strings.Contains(data, `"event":"answer"`) || !strings.Contains(data, `"verdict":"ask"`) {
		t.Fatalf("log:\n%s", data)
	}
	f.clock.Add(time.Hour)
	f.decide(egressReq("api.example.com", "93.184.216.34"))
	data = string(f.read(decisionsFile))
	if !strings.Contains(data, `"counts":{"api.example.com:443":50}`) {
		t.Fatalf("counts:\n%s", data)
	}
	// The crossings' lines are limited.
	for i := range logMax + 10 {
		f.decide(aliasReq(fmt.Sprintf("nope%d", i)))
	}
	f.clock.Add(time.Minute)
	f.decide(aliasReq("nope"))
	data = string(f.read(decisionsFile))
	if !strings.Contains(data, `"event":"suppressed"`) || strings.Count(data, `"rule":"deny:no-alias"`) != logMax+1 {
		t.Fatalf("limit: %d lines", strings.Count(data, "\n"))
	}
}

func TestAliases(t *testing.T) {
	f := newFixture(t)
	as, err := f.b.Aliases()
	if err != nil || len(as) != 2 || as[0].Name != "build" || !as[0].Asks || as[1].Asks {
		t.Fatalf("%+v %v", as, err)
	}
	a, ok, err := f.b.Alias("free")
	if err != nil || !ok || a.Command[0] != "/usr/bin/true" {
		t.Fatalf("%+v %v %v", a, ok, err)
	}
	if _, ok, _ := f.b.Alias("nope"); ok {
		t.Fatal("unknown alias found")
	}
}

func TestKeepDecided(t *testing.T) {
	d := requestsDoc{}
	for i := range KeepDecided + 50 {
		st := StateDenied
		if i%10 == 0 {
			st = StatePending
		}
		d.Requests = append(d.Requests, reqRec{ID: uint64(i + 1), State: st, DecidedAt: f2Time().Add(time.Duration(i) * time.Second)})
	}
	pruneDecided(&d)
	pending, decided := 0, 0
	for _, r := range d.Requests {
		if r.State == StatePending {
			pending++
		} else {
			decided++
		}
	}
	if pending != (KeepDecided+50)/10 || decided != KeepDecided || d.Requests[len(d.Requests)-1].ID != KeepDecided+50 {
		t.Fatalf("pending %d decided %d", pending, decided)
	}
}

// An answer whose grant was written but whose record was not leaves the
// request pending; answering it again makes no second grant.
func TestAnswerRetryAddsNoSecondGrant(t *testing.T) {
	f := newFixture(t)
	id := mustPending(t, f.decide(egressReq("db.corp.example", "10.0.0.5")))
	writeFault = func(name string) error {
		if name == requestsFile {
			return errors.New("disk full")
		}
		return nil
	}
	_, err := f.b.Answer(id, Reply{Verdict: ReplyAllow, Scope: ScopeHours, Hours: 2, By: ByCLI})
	writeFault = nil
	if err == nil {
		t.Fatal("an answer whose record could not be written succeeded")
	}
	if ri, _ := f.b.Request(id); ri.State != StatePending {
		t.Fatalf("after the failed write: %+v", ri)
	}
	res := f.answer(id, Reply{Verdict: ReplyAllow, Scope: ScopeHours, Hours: 2})
	gs, err := f.b.Grants()
	if err != nil {
		t.Fatal(err)
	}
	if len(gs) != 1 || res.Grant == "" || gs[0].ID != res.Grant {
		t.Fatalf("grants after the retry: %+v (answer named %q)", gs, res.Grant)
	}
	if ri, _ := f.b.Request(id); ri.State != StateAllowed {
		t.Fatalf("after the retry: %+v", ri)
	}
}

// Allowing a URL request for hours opens the URLs of the other pending
// requests it decides too, each once.
func TestAlsoOpensURLs(t *testing.T) {
	f := newFixture(t)
	a, _ := f.b.File(Ask{Request: urlReq("https://docs.example/a"), Session: "a"})
	b, _ := f.b.File(Ask{Request: urlReq("https://docs.example/b"), Session: "b"})
	c, _ := f.b.File(Ask{Request: urlReq("https://docs.example/a"), Session: "c"})
	id := mustPending(t, a)
	mustPending(t, b)
	mustPending(t, c)
	res := f.answer(id, Reply{Verdict: ReplyAllow, Scope: ScopeHours, Hours: 1})
	if len(res.Also) != 2 {
		t.Fatalf("also decided: %v", res.Also)
	}
	got := f.open.opened()
	slices.Sort(got)
	if want := []string{"https://docs.example/a", "https://docs.example/b"}; !slices.Equal(got, want) {
		t.Fatalf("opened %v, want %v", got, want)
	}
}

// Run's ticker expires nothing while this broker does not own the
// environment, and expires once it does.
func TestRunOwnerCheck(t *testing.T) {
	old := runEvery
	runEvery = 5 * time.Millisecond
	t.Cleanup(func() { runEvery = old })
	f := newFixture(t)
	id := mustPending(t, f.decide(aliasReq("build")))
	f.owner.lost.Store(true)
	f.clock.Add(RequestTTL + time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { f.b.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	time.Sleep(50 * time.Millisecond)
	if strings.Contains(string(f.read(requestsFile)), StateExpired) {
		t.Fatal("a broker that does not own the environment expired a request")
	}
	f.owner.lost.Store(false)
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(string(f.read(requestsFile)), `"`+StateExpired+`"`) {
		if time.Now().After(deadline) {
			t.Fatalf("request #%d never expired under Run", id)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// The writable set's identities decide an alias as its paths do: a
// command found, by its identity, in a writable directory is refused,
// whatever its path says.
func TestWritableIdentities(t *testing.T) {
	f := newFixture(t)
	w := t.TempDir()
	o := f.options()
	o.Writable = func() ([]string, error) { return []string{w}, nil }
	f.b.Close()
	b, err := Open(o)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	fi, err := os.Stat(w)
	if err != nil {
		t.Fatal(err)
	}
	id, ok := policy.IDOf(fi)
	if !ok {
		t.Skip("no file identities here")
	}
	r := aliasReq("free")
	if out, _ := b.Decide(r); out.Verdict != Allowed {
		t.Fatalf("a command outside the writable set: %+v", out)
	}
	r.CommandIDs = append(r.CommandIDs, id)
	if out, _ := b.Decide(r); out.Verdict != Denied || !strings.Contains(out.Reason, "by another name") {
		t.Fatalf("a command whose directory is writable by identity: %+v", out)
	}
	// A caller that gives no identities, where the set has some, is
	// refused: it did not look.
	r.CommandIDs = nil
	if out, _ := b.Decide(r); out.Verdict != Denied {
		t.Fatalf("a command with no identities: %+v", out)
	}
}

// An alias is decided on what the sandboxes can write now: the set is
// read again once what was read is a couple of seconds old, and no more
// often than that.
func TestWritableRefreshedForAliases(t *testing.T) {
	f := newFixture(t)
	var mu sync.Mutex
	set, reads := []string{"/nowhere"}, 0
	o := f.options()
	o.Writable = func() ([]string, error) {
		mu.Lock()
		defer mu.Unlock()
		reads++
		return slices.Clone(set), nil
	}
	f.b.Close()
	b, err := Open(o)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if out, _ := b.Decide(aliasReq("free")); out.Verdict != Allowed {
		t.Fatalf("before: %+v", out)
	}
	mu.Lock()
	set = []string{"/usr/bin"}
	n := reads
	mu.Unlock()
	if out, _ := b.Decide(aliasReq("free")); out.Verdict != Allowed {
		t.Fatalf("within the TTL the set read stands: %+v", out)
	}
	f.clock.Add(writableTTL)
	if out, _ := b.Decide(aliasReq("free")); out.Verdict != Denied {
		t.Fatalf("after the TTL, a command now within the writable set: %+v", out)
	}
	mu.Lock()
	defer mu.Unlock()
	if reads != n+1 {
		t.Errorf("%d reads of the writable set for two decisions, want 1", reads-n)
	}
	// Egress and URLs never read it.
	_, _ = b.Decide(egressReq("api.example.com", "93.184.216.34"))
	if reads != n+1 {
		t.Errorf("an egress decision read the writable set")
	}
}
