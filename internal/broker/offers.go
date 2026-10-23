package broker

import (
	"strings"

	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/policy"
)

// Bounds of an "always" offer's diff: what a person can be shown whole
// and asked to confirm. A change past them is not offered.
const (
	MaxOfferDiffLines = 200
	MaxOfferDiffBytes = 64 << 10
)

// HoursPlaceholder stands for the number of hours in an "hours" offer's
// effect, which the person picks.
const HoursPlaceholder = "{hours}"

// Offer is one way of allowing a request that the host offers, with what
// it does in the host's words.
type Offer struct {
	// Scope is ScopeOnce, ScopeHours or ScopeAlways.
	Scope string
	// Effect is what allowing so does (policy.AnswerEffect); for
	// ScopeHours it holds HoursPlaceholder where the hours go.
	Effect string
	// HoursMin and HoursMax bound an "hours" answer.
	HoursMin, HoursMax int
	// Diff and DiffHash are, for ScopeAlways, the change to config.toml,
	// exactly as it would be written now, and the hash an answer confirms
	// it by.
	Diff, DiffHash string

	edit config.Edit // ScopeAlways's edit, for Answer
}

// Withheld is a way of allowing a request that the host does not offer,
// and why.
type Withheld struct {
	Scope, Why string
}

// offerScopes are the scopes an allow may have, in the order offered.
var offerScopes = []string{ScopeOnce, ScopeHours, ScopeAlways}

// RequestOffers is request id in full, with the ways of allowing it the
// host offers now and those it does not. A request that is not pending
// has neither. An answer that allows is refused unless its scope is
// among the offers it would get then.
func (b *Broker) RequestOffers(id uint64) (RequestInfo, []Offer, []Withheld, error) {
	ri, err := b.Request(id)
	if err != nil {
		return ri, nil, nil, err
	}
	b.mu.Lock()
	var q reqRec
	if p := b.st.reqs.find(id); p != nil {
		q = *p
	}
	b.mu.Unlock()
	if q.ID == 0 || q.State != StatePending {
		return ri, nil, nil, nil
	}
	_, set := b.snapshot(b.now())
	var offers []Offer
	var withheld []Withheld
	for _, scope := range offerScopes {
		o, err := b.offer(&q, set, scope)
		if err != nil {
			withheld = append(withheld, Withheld{Scope: scope, Why: errText(err)})
			continue
		}
		offers = append(offers, o)
	}
	return ri, offers, withheld, nil
}

// errText is err's message, without the broker's kind.
func errText(err error) string {
	if e, ok := err.(*Error); ok {
		return e.Msg
	}
	return err.Error()
}

// offer is the offer of scope for q, or why it is not made, as an error
// an answer of that scope gets.
func (b *Broker) offer(q *reqRec, set Settings, scope string) (Offer, error) {
	r := q.request()
	if r.Kind == policy.KindOpenURL && b.opts.Opener == nil {
		return Offer{}, newError(KindUnavailable, "this host has nothing to open URLs with, so a URL request cannot be allowed")
	}
	o := Offer{Scope: scope}
	switch scope {
	case ScopeOnce:
		o.Effect = policy.AnswerEffect(r, policy.ScopeOnce, 0, b.opts.Env, set.VMProfile, set.ConfigPath)
	case ScopeHours:
		o.HoursMin, o.HoursMax = 1, MaxHours
		// The effect ends "for 1 hour" (policy.AnswerEffect): the hours
		// are the person's to pick.
		e := policy.AnswerEffect(r, policy.ScopeHours, 1, b.opts.Env, set.VMProfile, set.ConfigPath)
		if s, ok := strings.CutSuffix(e, " for 1 hour"); ok {
			o.Effect = s + " for " + HoursPlaceholder + " hours"
		}
	case ScopeAlways:
		o.Effect = policy.AnswerEffect(r, policy.ScopeAlways, 0, b.opts.Env, set.VMProfile, set.ConfigPath)
		if o.Effect == "" {
			break
		}
		e, err := b.alwaysEdit(q, set)
		if err != nil {
			return Offer{}, err
		}
		diff, err := b.opts.ConfigWriter.Diff(e)
		if err != nil {
			return Offer{}, newError(KindUnavailable, "cannot make the change to %s: %v", set.ConfigPath, err)
		}
		if len(diff) > MaxOfferDiffBytes || strings.Count(strings.TrimSuffix(diff, "\n"), "\n") >= MaxOfferDiffLines {
			return Offer{}, newError(KindUnavailable, "the change to %s is over %d lines or %d bytes: too long to show, so not offered", set.ConfigPath, MaxOfferDiffLines, MaxOfferDiffBytes)
		}
		o.Diff, o.DiffHash, o.edit = diff, DiffHash(diff), e
	default:
		return Offer{}, newError(KindBadRequest, "an answer's scope is %q, %q or %q, not %q", ScopeOnce, ScopeHours, ScopeAlways, clip(scope, 32))
	}
	if o.Effect == "" {
		return Offer{}, newError(KindBadRequest, "a request of kind %q cannot be allowed %s", clip(string(q.Kind), 32), scope)
	}
	return o, nil
}
