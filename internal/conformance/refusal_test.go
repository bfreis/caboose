package conformance_test

import (
	"context"
	"strings"
	"testing"

	c "github.com/bfreis/caboose/internal/conformance"
	"github.com/bfreis/caboose/internal/conformance/fake"
)

// scripted wraps a fake sandbox and lets a test change what the attempt
// (the second command of a check) appears to say, or what really happens.
type scripted struct {
	*fake.Fake
	n      int
	script func(n int, argv []string, real func() (int, string, string, error)) (int, string, string, error)
}

func (s *scripted) Exec(ctx context.Context, user string, argv []string, stdin []byte) (int, string, string, error) {
	s.n++
	n := s.n
	return s.script(n, argv, func() (int, string, string, error) { return s.Fake.Exec(ctx, user, argv, stdin) })
}

func runRefusal(t *testing.T, mut fake.Mutant, ck c.Check, script func(n int, argv []string, real func() (int, string, string, error)) (int, string, string, error)) c.Result {
	t.Helper()
	f, env := newWorld(t, isoContainer)
	f.Break(mut)
	sb := &scripted{Fake: f, script: script}
	e := c.NewEnv(sb, f, f, f, isoContainer, "fallible")
	r := &c.Runner{Env: env, Wipe: true}
	res, _, err := c.RunChecks(context.Background(), r, []c.Check{ck}, e)
	if err != nil {
		t.Fatal(err)
	}
	return res[0]
}

func say(s string, rc int) (int, string, string, error) {
	return 0, "\nrc=" + string(rune('0'+rc)) + "\n", s, nil
}

func TestRefusalVerdicts(t *testing.T) {
	f1 := refusals()[0] // append to ro-file, erofs
	pass := func(_ int, _ []string, real func() (int, string, string, error)) (int, string, string, error) {
		return real()
	}
	// 1 is the positive control, 2 the attempt.
	for _, tc := range []struct {
		name   string
		mut    fake.Mutant
		script func(int, []string, func() (int, string, string, error)) (int, string, string, error)
		want   c.Status
		reason string
	}{
		{"refused", fake.Enforcing, pass, c.Pass, ""},
		{"the sandbox says refused but the host sees a change", fake.RootsWritable,
			func(n int, a []string, real func() (int, string, string, error)) (int, string, string, error) {
				rc, out, e, err := real()
				if n == 2 {
					return say("sh: 1: cannot create x: Read-only file system", 2)
				}
				return rc, out, e, err
			}, c.Fail, "changed on the host"},
		{"the sandbox says success and nothing changed", fake.Enforcing,
			func(n int, a []string, real func() (int, string, string, error)) (int, string, string, error) {
				if n == 2 {
					return say("", 0)
				}
				return real()
			}, c.Fail, "reported success"},
		{"the sandbox says success to the control and nothing changed", fake.Enforcing,
			func(n int, a []string, real func() (int, string, string, error)) (int, string, string, error) {
				if n == 1 {
					return say("", 0)
				}
				return real()
			}, c.Error, "did not change"},
		{"no such file where a refusal was expected", fake.Enforcing,
			func(n int, a []string, real func() (int, string, string, error)) (int, string, string, error) {
				if n == 2 {
					return say("sh: 1: cannot create x: No such file or directory", 2)
				}
				return real()
			}, c.Error, "not a refusal"},
		{"no message", fake.Enforcing,
			func(n int, a []string, real func() (int, string, string, error)) (int, string, string, error) {
				if n == 2 {
					return say("", 1)
				}
				return real()
			}, c.Error, "not a refusal"},
		{"the shell did not run the wrapper", fake.Enforcing,
			func(n int, a []string, real func() (int, string, string, error)) (int, string, string, error) {
				if n == 2 {
					return 127, "", "exec: no such file", nil
				}
				return real()
			}, c.Error, "not a refusal"},
		{"refused as eacces where only erofs is expected", fake.Enforcing,
			func(n int, a []string, real func() (int, string, string, error)) (int, string, string, error) {
				if n == 2 {
					return say("sh: 1: cannot create x: Permission denied", 2)
				}
				return real()
			}, c.Fail, "expected erofs"},
		{"the transport fails on the attempt", fake.Enforcing,
			func(n int, a []string, real func() (int, string, string, error)) (int, string, string, error) {
				if n == 2 {
					return -1, "", "", context.DeadlineExceeded
				}
				return real()
			}, c.Error, "could not be run"},
		{"the positive control is refused", fake.Enforcing,
			func(n int, a []string, real func() (int, string, string, error)) (int, string, string, error) {
				if n == 1 {
					return say("sh: 1: cannot create x: Read-only file system", 2)
				}
				return real()
			}, c.Error, "positive control failed"},
	} {
		r := runRefusal(t, tc.mut, f1, tc.script)
		if r.Status != tc.want || !strings.Contains(r.Reason, tc.reason) {
			t.Errorf("%s: %s %q, want %s containing %q", tc.name, r.Status, r.Reason, tc.want, tc.reason)
		}
	}
}

func TestRefusalAcceptsListedClasses(t *testing.T) {
	ck := c.Refusal{ID: "A1", Group: "files", Class: c.Write, User: "agent", Target: "ro-file", Allowed: "wr-file",
		Attempt: c.OpAppend, Expect: []c.ErrClass{c.ClassErofs, c.ClassEacces}}.Check()
	r := runRefusal(t, fake.Enforcing, ck, func(n int, a []string, real func() (int, string, string, error)) (int, string, string, error) {
		if n == 2 {
			return say("sh: 1: cannot create x: Permission denied", 2)
		}
		return real()
	})
	if r.Status != c.Pass {
		t.Errorf("%s %s", r.Status, r.Reason)
	}
}

func TestRefusalValidate(t *testing.T) {
	base := c.Refusal{ID: "A1", Group: "g", Class: c.Write, User: "agent", Target: "ro-file", Allowed: "wr-file", Attempt: c.OpAppend, Expect: []c.ErrClass{c.ClassErofs}}
	for name, mod := range map[string]func(*c.Refusal){
		"enoent expected":          func(r *c.Refusal) { r.Expect = []c.ErrClass{c.ClassEnoent} },
		"other expected":           func(r *c.Refusal) { r.Expect = []c.ErrClass{c.ClassOther} },
		"ok expected":              func(r *c.Refusal) { r.Expect = []c.ErrClass{c.ClassOK} },
		"nothing expected":         func(r *c.Refusal) { r.Expect = nil },
		"same canary":              func(r *c.Refusal) { r.Allowed = r.Target },
		"an observe check":         func(r *c.Refusal) { r.Class = c.Observe },
		"no operation":             func(r *c.Refusal) { r.Attempt = c.Op{} },
		"no positive control":      func(r *c.Refusal) { r.Allowed = "" },
		"verified without a state": func(r *c.Refusal) { r.Verified = []c.ErrClass{c.ClassEnotempty} },
		"other verified": func(r *c.Refusal) {
			r.State = func(context.Context, c.Env, string, c.SPath) (string, string, error) { return "s", "", nil }
			r.Verified = []c.ErrClass{c.ClassOther}
		},
		"ok verified": func(r *c.Refusal) {
			r.State = func(context.Context, c.Env, string, c.SPath) (string, string, error) { return "s", "", nil }
			r.Verified = []c.ErrClass{c.ClassOK}
		},
	} {
		r := base
		mod(&r)
		if r.Validate() == nil {
			t.Errorf("%s: accepted", name)
		}
		f, env := newWorld(t, isoContainer)
		res, _, err := runAll(t, f, env, []c.Check{r.Check(), anchorCheck()}, isoContainer, true)
		if err != nil {
			t.Fatal(err)
		}
		if st, _ := statusOf(res, "A1"); st != c.Error {
			t.Errorf("%s: ran as %s", name, st)
		}
		if len(f.Events()) > 1 && containsAny(f.Events(), "exec") {
			t.Errorf("%s: the sandbox was used", name)
		}
	}
}

func anchorCheck() c.Check {
	return c.Check{ID: "anchor", Group: "g", Class: c.Observe,
		Run: func(context.Context, c.Env) c.CheckResult { return c.CheckResult{Status: c.Pass} }}
}

// A State read before and after the attempt: the classes in Verified
// count only when it held both times, unchanged; bad, a change or an
// unreadable state is never a PASS.
func TestRefusalState(t *testing.T) {
	enoent := func(n int, a []string, real func() (int, string, string, error)) (int, string, string, error) {
		if n == 2 {
			return say("sh: 1: cannot create x: No such file or directory", 2)
		}
		return real()
	}
	for _, tc := range []struct {
		name   string
		state  func(n int) (string, string, error)
		want   c.Status
		reason string
	}{
		{"held", func(int) (string, string, error) { return "m", "", nil }, c.Pass, ""},
		{"bad before", func(int) (string, string, error) { return "", "no mount there", nil }, c.Fail, "before the attempt: no mount there"},
		{"bad after", func(n int) (string, string, error) {
			if n == 2 {
				return "", "no mount there", nil
			}
			return "m", "", nil
		}, c.Fail, "after the attempt"},
		{"changed", func(n int) (string, string, error) { return []string{"", "m", "n"}[n], "", nil }, c.Fail, "the mount changed"},
		{"unreadable", func(n int) (string, string, error) {
			if n == 2 {
				return "", "", context.DeadlineExceeded
			}
			return "m", "", nil
		}, c.Error, "after the attempt"},
	} {
		n := 0
		ck := c.Refusal{ID: "A1", Group: "files", Class: c.Write, User: "agent", Target: "ro-file", Allowed: "wr-file",
			Attempt: c.OpAppend, Expect: []c.ErrClass{c.ClassErofs}, Verified: []c.ErrClass{c.ClassEnoent},
			State: func(context.Context, c.Env, string, c.SPath) (string, string, error) { n++; return tc.state(n) }}.Check()
		r := runRefusal(t, fake.Enforcing, ck, enoent)
		if r.Status != tc.want || !strings.Contains(r.Reason, tc.reason) {
			t.Errorf("%s: %s %q, want %s containing %q", tc.name, r.Status, r.Reason, tc.want, tc.reason)
		}
	}
	// Without a State, ENOENT is no refusal.
	ck := c.Refusal{ID: "A1", Group: "files", Class: c.Write, User: "agent", Target: "ro-file", Allowed: "wr-file",
		Attempt: c.OpAppend, Expect: []c.ErrClass{c.ClassErofs}}.Check()
	if r := runRefusal(t, fake.Enforcing, ck, enoent); r.Status != c.Error {
		t.Errorf("no state: %s %s", r.Status, r.Reason)
	}
}
