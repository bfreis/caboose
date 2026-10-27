package hostlink

// Requests. With a broker behind the link (Config.Decide), the hello
// offers OpBrokerRequest, which files a request ahead of time, and
// OpBrokerRequests, which lists requests and waits for decisions. Both
// are the sandbox's to send as often as it likes, so both are bounded:
// filing takes a token from the request bucket open and notify share;
// listing has a bucket of its own; and at most maxWaits waits are open on
// a link at once, each in its own goroutine, so a wait holds up neither
// the control loop nor any other request. Nothing here decides a request:
// the broker files, merges and lists them, and only the host's own
// clients answer.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"time"

	"github.com/bfreis/caboose/internal/agentproto"
	"github.com/bfreis/caboose/internal/broker"
	"github.com/bfreis/caboose/internal/policy"
)

const (
	// maxWaits is the most OpBrokerRequests waiting at once on one link,
	// whatever sessions they claim: the claimed session is the sandbox's
	// word, and caps nothing.
	maxWaits = broker.MaxWaitsPerLink
	// listEvery and listBurst bound how often the sandbox may list.
	listEvery = time.Second
	listBurst = 20
	// The most of each text a listed request carries back: the
	// response must fit one control message.
	maxListedEffect = 300
	maxListedReason = 300
	// listBudget is how many bytes a response's requests may take.
	listBudget = agentproto.MaxPayload - 4<<10
)

// requestsState is a Host's bucket for OpBrokerRequests and its count of
// the waits open.
type requestsState struct {
	tokens float64
	last   time.Time
	waits  int
}

// errNoBroker is a request for the broker on a link that has none.
var errNoBroker = errors.New("this host files no requests: nothing on it answers them")

// brokerRequest serves OpBrokerRequest: the crossing a checked, with the
// facts the host resolves for it itself, filed with the broker with the
// sandbox's words; r gets the verdict.
func (h *Host) brokerRequest(a *agentproto.Ask, r *agentproto.Message) error {
	if h.cfg.Decide == nil {
		return errNoBroker
	}
	if a == nil {
		return errors.New("the request names nothing to ask for")
	}
	if err := checkAsk(a); err != nil {
		return err
	}
	// A port this link forwards is already as the sandbox wants: nothing
	// to file, and nothing to check again.
	if policy.Kind(a.Kind) == policy.KindPort && h.forwarding(a.Port) {
		r.Verdict = agentproto.VerdictAllowed
		return nil
	}
	req, err := h.askRequest(a)
	if err != nil {
		return err
	}
	o, err := h.cfg.Decide.File(broker.Ask{Request: req, Context: a.Context, Options: a.Options,
		Recommendation: a.Recommendation, Meanwhile: a.Meanwhile, Session: a.Session})
	if err != nil && o.Verdict == "" {
		return err
	}
	return verdict(o, r)
}

// verdict puts o in r: its Verdict always, and, unless it allows, its
// reason as the error, with the request's number when pending.
func verdict(o broker.Outcome, r *agentproto.Message) error {
	switch o.Verdict {
	case broker.Allowed:
		r.Verdict = agentproto.VerdictAllowed
		return nil
	case broker.Pending:
		r.Verdict = agentproto.VerdictPending
		return &refusal{reason: agentproto.ReasonPending, request: o.Request, msg: fmt.Sprintf("pending #%d: %s", o.Request, o.Reason)}
	}
	r.Verdict = agentproto.VerdictDenied
	return errors.New(o.Reason)
}

// checkAsk bounds what the sandbox says of a request before anything
// reads it; the broker checks the text itself again.
func checkAsk(a *agentproto.Ask) error {
	switch {
	case len(a.Context) > broker.MaxContext:
		return fmt.Errorf("context is longer than %d bytes", broker.MaxContext)
	case len(a.Options) > broker.MaxOptions:
		return fmt.Errorf("at most %d options", broker.MaxOptions)
	case len(a.Recommendation) > broker.MaxRecommendation:
		return fmt.Errorf("recommendation is longer than %d bytes", broker.MaxRecommendation)
	case len(a.Meanwhile) > broker.MaxMeanwhile:
		return fmt.Errorf("meanwhile is longer than %d bytes", broker.MaxMeanwhile)
	case len(a.Session) > broker.MaxSession:
		return fmt.Errorf("session is longer than %d bytes", broker.MaxSession)
	case len(a.Args) > policy.MaxArgs:
		return fmt.Errorf("at most %d arguments", policy.MaxArgs)
	}
	for i, o := range a.Options {
		if len(o) > broker.MaxOption {
			return fmt.Errorf("option %d is longer than %d bytes", i+1, broker.MaxOption)
		}
	}
	return nil
}

// askRequest is the crossing a asks for, with what the host resolved for
// it: never an address or a path the sandbox chose.
func (h *Host) askRequest(a *agentproto.Ask) (policy.Request, error) {
	switch policy.Kind(a.Kind) {
	case policy.KindOpenURL:
		u, err := CheckURL(a.URL)
		if err != nil {
			return policy.Request{}, err
		}
		return policy.Request{Kind: policy.KindOpenURL, URL: u, Origin: Origin(u)}, nil
	case policy.KindEgress:
		e := h.cfg.Egress
		if e == nil {
			return policy.Request{}, errors.New("the host offers no outbound proxy (egress in the vm profile of config.toml on the host)")
		}
		name, addr, err := checkEgressHost(a.Host)
		if err != nil {
			return policy.Request{}, err
		}
		if !agentproto.ValidPort(a.Port) {
			return policy.Request{}, fmt.Errorf("%d is not a port", a.Port)
		}
		target := net.JoinHostPort(name, strconv.Itoa(a.Port))
		if !e.Ports.Has(a.Port) {
			return policy.Request{}, fmt.Errorf("%s: port %d is not in egress_ports (config.toml on the host)", target, a.Port)
		}
		addrs := []netip.Addr{addr}
		if !addr.IsValid() {
			if addrs, err = e.resolve(name); err != nil {
				return policy.Request{}, err
			}
		}
		return policy.Request{Kind: policy.KindEgress, Host: name, Literal: addr.IsValid(), Port: a.Port,
			Addrs: addrs, Own: h.ownAddrs()}, nil
	case policy.KindPort:
		return h.askPort(a.Port)
	case policy.KindAlias:
		if !h.aliasesOn() {
			return policy.Request{}, errNoAliases
		}
		if len(a.Args) > agentproto.MaxAliasArgs {
			return policy.Request{}, fmt.Errorf("at most %d arguments", agentproto.MaxAliasArgs)
		}
		argv := []string{a.Alias}
		for k, v := range a.Args {
			argv = append(argv, k+"="+v)
		}
		name, args, err := agentproto.ParseAliasArgv(argv)
		if err != nil {
			return policy.Request{}, err
		}
		al, ok, err := h.cfg.Decide.Alias(name)
		if err != nil {
			return policy.Request{}, err
		}
		if !ok {
			// The policy refuses it: no such alias.
			return policy.Request{Kind: policy.KindAlias, Alias: name, Args: args}, nil
		}
		// What the policy compares, resolved now, as a run resolves it.
		_, req, err := resolveAlias(al, args)
		if err != nil {
			return policy.Request{}, err
		}
		return req, nil
	}
	return policy.Request{}, fmt.Errorf("the host does not know requests of the kind %q", clip(printableHost(a.Kind), 32))
}

// takeList takes a token from the listing bucket.
func (h *Host) takeList() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	g := &h.reqs
	now := time.Now()
	if g.last.IsZero() {
		g.tokens = listBurst
	} else {
		g.tokens = min(listBurst, g.tokens+now.Sub(g.last).Seconds()/listEvery.Seconds())
	}
	g.last = now
	if g.tokens < 1 {
		return false
	}
	g.tokens--
	return true
}

// brokerRequests serves OpBrokerRequests into r: the requests the filter
// m names, at once, or once a decision newer than m.Since exists for it,
// or m.Wait seconds have passed.
func (h *Host) brokerRequests(m agentproto.Message, r *agentproto.Message) error {
	if h.cfg.Decide == nil {
		return errNoBroker
	}
	if !h.takeList() {
		return errors.New("too many requests; try again shortly")
	}
	if len(m.Session) > broker.MaxSession {
		return fmt.Errorf("session is longer than %d bytes", broker.MaxSession)
	}
	wait := min(max(m.Wait, 0), agentproto.MaxWait)
	q := broker.Query{Session: m.Session, All: m.All, Since: m.Since, Limit: agentproto.MaxRequests}
	if wait == 0 {
		reqs, cur, err := h.cfg.Decide.List(q)
		if err != nil {
			return err
		}
		listed(r, reqs, cur, m.Since)
		return nil
	}
	h.mu.Lock()
	if h.reqs.waits >= maxWaits {
		h.mu.Unlock()
		return fmt.Errorf("too many waits: at most %d at once on one link", maxWaits)
	}
	h.reqs.waits++
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		h.reqs.waits--
		h.mu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(wait)*time.Second)
	defer cancel()
	// The wait ends with the session, too.
	go func() {
		select {
		case <-h.sess.Done():
			cancel()
		case <-ctx.Done():
		}
	}()
	for {
		reqs, cur, err := h.cfg.Decide.List(q)
		if err != nil {
			return err
		}
		if ctx.Err() != nil || decidedSince(reqs, m.Since) {
			listed(r, reqs, cur, m.Since)
			return nil
		}
		if _, err := h.cfg.Decide.Wait(ctx, cur); err != nil {
			return err
		}
	}
}

// decidedSince reports whether any of reqs was decided after the cursor
// since.
func decidedSince(reqs []broker.RequestInfo, since uint64) bool {
	for _, q := range reqs {
		if q.State != broker.StatePending && q.Seq > since {
			return true
		}
	}
	return false
}

// listed puts reqs, newest first, and the cursor cur in r, within what
// one control message carries. The decisions made after since come first
// (broker.Fit): what is left out is the newest of them, and the cursor
// goes back to just before it, so a caller waiting from it is told of it
// next, and of nothing it already had.
func listed(r *agentproto.Message, reqs []broker.RequestInfo, cur, since uint64) {
	used := 0
	wire := func(q broker.RequestInfo) agentproto.RequestInfo {
		return agentproto.RequestInfo{ID: q.ID, Kind: string(q.Kind), Effect: clip(q.Effect, maxListedEffect), State: q.State,
			Scope: q.Scope, Hours: q.Hours, Expires: q.Expires, DecidedAt: q.DecidedAt,
			Reason: clip(q.Reason, maxListedReason), Session: q.Session, Seq: q.Seq}
	}
	kept, cur := broker.Fit(reqs, since, cur, func(q broker.RequestInfo) bool {
		b, _ := json.Marshal(wire(q))
		if used+len(b)+1 > listBudget {
			return false
		}
		used += len(b) + 1
		return true
	}, agentproto.MaxRequests)
	r.Requests = nil
	for _, q := range kept {
		r.Requests = append(r.Requests, wire(q))
	}
	r.Cursor = cur
}

// mods keeps the agent's latest count of sessions and of those with
// caboose's mod, and passes it on when it changed. The agent says what it
// likes: a count that is no count is dropped.
func (h *Host) mods(m *agentproto.ModsInfo) {
	const most = 1 << 20
	if m == nil || m.Sessions < 0 || m.WithMod < 0 || m.Sessions > most || m.WithMod > m.Sessions {
		return
	}
	h.mu.Lock()
	same := h.lastMods != nil && *h.lastMods == *m
	got := *m
	h.lastMods = &got
	h.mu.Unlock()
	if !same && h.cfg.Mods != nil {
		h.cfg.Mods(got)
	}
}
