package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/daemonproto"
)

// fakeProposals answers the proposal methods with what it holds, and
// records the calls.
type fakeProposals struct {
	mu    sync.Mutex
	calls []string
	list  *daemonproto.ProposalsResult
	err   error
}

func (f *fakeProposals) note(s string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, s)
	return f.err
}

func (f *fakeProposals) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeProposals) Proposals(_ context.Context, env string) (*daemonproto.ProposalsResult, error) {
	if err := f.note("proposals " + env); err != nil {
		return nil, err
	}
	if f.list == nil {
		return &daemonproto.ProposalsResult{Proposals: []daemonproto.ProposalInfo{}}, nil
	}
	return f.list, nil
}

func (f *fakeProposals) ProposalDelete(_ context.Context, env, file, hash string) error {
	return f.note("delete " + env + " " + file + " " + hash)
}

func (f *fakeProposals) ProposalApply(_ context.Context, p daemonproto.ProposalApplyParams) (*daemonproto.ProposalApplyResult, error) {
	if err := f.note("apply " + p.Env + " " + p.File + " " + p.Typed); err != nil {
		return nil, err
	}
	return &daemonproto.ProposalApplyResult{Applied: true, Notes: []string{"restart"}}, nil
}

func (f *fakeProposals) ProposalBuild(_ context.Context, p daemonproto.ProposalApplyParams) (*daemonproto.JobInfo, error) {
	if err := f.note("build " + p.Env + " " + p.File + " " + p.Typed); err != nil {
		return nil, err
	}
	return &daemonproto.JobInfo{Kind: daemonproto.JobApply, State: daemonproto.JobRunning, Since: time.Now().UTC(), Proposal: p.File}, nil
}

// The proposal methods are served to every client kind; a daemon given
// none does not know them; bad params never reach the service, its errors
// come back as they are, and a list the daemon cannot vouch for is never
// sent.
func TestProposalMethods(t *testing.T) {
	none := startCheckpointDaemon(t, nil)
	if _, err := dialAs(t, none, daemonproto.ClientCLI, 0).Proposals(t.Context(), "alpha"); daemonproto.Kind(err) != daemonproto.KindUnknownMethod {
		t.Fatalf("without Proposals: %v", err)
	}
	h := strings.Repeat("ab", 32)
	f := &fakeProposals{list: &daemonproto.ProposalsResult{Proposals: []daemonproto.ProposalInfo{{File: "foo.toml", Modified: time.Now(),
		Title: "<b>t</b>", Parts: []daemonproto.ProposalPart{{Kind: "root", Diff: []string{"+ a"}, Summary: "s"}},
		WebApply: true, Confirm: daemonproto.ProposalConfirm{Kind: "type_name", Name: "foo"}, Hash: h}}}}
	dir, s := startServer(t, Options{Version: "v1.0.0", Idle: time.Minute, Log: testLog{t}, Proposals: f})
	for _, kind := range []string{daemonproto.ClientCLI, daemonproto.ClientApp} {
		c := dialAs(t, dir, kind, 0)
		if r, err := c.Proposals(t.Context(), "alpha"); err != nil || len(r.Proposals) != 1 || r.Proposals[0].Title != "<b>t</b>" {
			t.Fatalf("%s proposals: %+v, %v", kind, r, err)
		}
		if err := c.ProposalDelete(t.Context(), "alpha", "foo.toml", h); err != nil {
			t.Fatal(err)
		}
		if r, err := c.ProposalApply(t.Context(), daemonproto.ProposalApplyParams{Env: "alpha", File: "foo.toml", Hash: h, Typed: "foo"}); err != nil || !r.Applied {
			t.Fatalf("%s apply: %+v, %v", kind, r, err)
		}
		if r, err := c.ProposalBuild(t.Context(), daemonproto.ProposalApplyParams{Env: "alpha", File: "foo.toml", Hash: h}); err != nil ||
			r.Job.Kind != daemonproto.JobApply || r.Job.Proposal != "foo.toml" {
			t.Fatalf("%s build: %+v, %v", kind, r, err)
		}
	}
	for name, params := range map[string]string{daemonproto.MethodProposals: `{"env":"alpha"}`,
		daemonproto.MethodProposalDelete: `{"env":"alpha","file":"foo.toml","hash":"` + h + `"}`,
		daemonproto.MethodProposalApply:  `{"env":"alpha","file":"foo.toml","hash":"` + h + `","typed":"foo"}`,
		daemonproto.MethodProposalBuild:  `{"env":"alpha","file":"foo.toml","hash":"` + h + `"}`} {
		raw, e := s.callWeb(t.Context(), name, json.RawMessage(params))
		if e != nil {
			t.Fatalf("web %s: %s, %v", name, raw, e)
		}
	}

	before := f.count()
	c := dialAs(t, dir, daemonproto.ClientCLI, 0)
	for _, bad := range []struct {
		method string
		params any
	}{
		{daemonproto.MethodProposals, map[string]any{"env": "../x"}},
		{daemonproto.MethodProposalDelete, map[string]any{"env": "alpha", "file": "../config.toml"}},
		{daemonproto.MethodProposalDelete, map[string]any{"env": "alpha", "file": "a/b.toml"}},
		{daemonproto.MethodProposalDelete, map[string]any{"env": "alpha", "file": "foo.toml"}},
		{daemonproto.MethodProposalDelete, map[string]any{"env": "alpha", "file": "x"}},
		{daemonproto.MethodProposalApply, map[string]any{"env": "alpha", "file": "foo.toml"}},
		{daemonproto.MethodProposalApply, map[string]any{"env": "alpha", "file": "foo.toml", "hash": h, "typed": "a\x1b"}},
		{daemonproto.MethodProposalBuild, map[string]any{"env": "alpha", "file": "foo.toml"}},
		{daemonproto.MethodProposalBuild, map[string]any{"env": "alpha", "file": "../x.toml", "hash": h}},
	} {
		if err := c.Call(t.Context(), bad.method, bad.params, nil); daemonproto.Kind(err) != daemonproto.KindBadRequest {
			t.Errorf("%s %v: %v", bad.method, bad.params, err)
		}
	}
	if f.count() != before {
		t.Errorf("bad params reached the service")
	}

	f.mu.Lock()
	f.err = daemonproto.NewError(daemonproto.KindStaleDiff, "changed")
	f.mu.Unlock()
	if _, err := c.ProposalApply(t.Context(), daemonproto.ProposalApplyParams{Env: "alpha", File: "foo.toml", Hash: h}); daemonproto.Kind(err) != daemonproto.KindStaleDiff {
		t.Errorf("the service's error: %v", err)
	}
	f.mu.Lock()
	f.err = nil
	f.list.Proposals[0].Title = "a\x1b[2J"
	f.mu.Unlock()
	if _, err := c.Proposals(t.Context(), "alpha"); daemonproto.Kind(err) != daemonproto.KindInternal {
		t.Errorf("a list with a control character: %v", err)
	}
}
