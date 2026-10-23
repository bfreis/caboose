package hostlink

import (
	"context"
	"time"

	"github.com/bfreis/caboose/internal/broker"
	"github.com/bfreis/caboose/internal/policy"
)

// Decider makes the host's decisions for a link: what the sandbox may have
// this machine do (open a URL, connect through the outbound proxy, run a
// host command), and the requests it files and waits on. Config.Decide is
// one, the environment's broker as one link sees it; nil decides as the
// Config's own settings say, and files nothing. What is offered at all
// (host exec, the outbound proxy, an SSH agent) stays the Config's: a
// Decider only answers for what an offer lets the sandbox ask.
type Decider = broker.Decider

// noBroker is the reason a link without a broker gives for what would
// otherwise wait for a person.
const noBroker = "and nothing on this host files requests for a person to answer"

// settingsDecider decides as a Config's settings say, through the same
// policy a broker uses, with no grants: what asks is refused, since no
// one would answer.
type settingsDecider struct{ p policy.Policy }

// The trust is the Config's, and anything but "fallible" (unset
// included) is decided as hostile, so a link that does not know the
// environment's trust refuses host exec, asks before opening a URL and
// asks (so, refuses) before any egress no entry names.
func newSettingsDecider(cfg *Config) settingsDecider {
	p := policy.Policy{Trust: policy.Trust(cfg.Trust), HostExec: cfg.HostExec != nil, OpenURLs: cfg.OpenURL}
	if e := cfg.Egress; e != nil {
		p.Egress = &policy.Egress{Ports: e.Ports, Allow: e.Allow, Deny: e.Deny}
	}
	return settingsDecider{p}
}

func (d settingsDecider) Decide(r policy.Request) (broker.Outcome, error) {
	dec := policy.Decide(d.p, r, time.Now())
	switch dec.Verdict {
	case policy.VerdictAllow:
		return broker.Outcome{Verdict: broker.Allowed, Reason: dec.Reason}, nil
	case policy.VerdictAsk:
		return broker.Outcome{Verdict: broker.Denied, Reason: dec.Reason + "; " + noBroker}, nil
	}
	return broker.Outcome{Verdict: broker.Denied, Reason: dec.Reason}, nil
}

func (d settingsDecider) File(broker.Ask) (broker.Outcome, error) {
	return broker.Outcome{Verdict: broker.Denied, Reason: "the request is refused: nothing on this host files requests for a person to answer"}, nil
}

func (d settingsDecider) List(broker.Query) ([]broker.RequestInfo, uint64, error) {
	return nil, 0, broker.ErrUnavailable
}

func (d settingsDecider) Wait(context.Context, uint64) (uint64, error) {
	return 0, broker.ErrUnavailable
}

func (d settingsDecider) Aliases() ([]broker.AliasInfo, error) { return nil, nil }

func (d settingsDecider) Alias(string) (policy.Alias, bool, error) { return policy.Alias{}, false, nil }

// decider is the Host's Decider: Config.Decide, or its settings'.
func (h *Host) decider() Decider {
	if h.cfg.Decide != nil {
		return h.cfg.Decide
	}
	return newSettingsDecider(&h.cfg)
}
