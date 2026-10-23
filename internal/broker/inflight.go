package broker

// The reasons a request in flight is denied with when whoever showed its
// dialog went away: never asked again another way, since the person may
// have been about to deny it, and a second dialog trains click-through.
const (
	ReasonAppGone    = "the app went away; ask again"
	ReasonDialogGone = "the dialog went away; ask again"
)

// InFlightReason is the reason a request in flight with by is denied
// with when by goes away.
func InFlightReason(by string) string {
	if by == ByApp {
		return ReasonAppGone
	}
	return ReasonDialogGone
}

func checkInFlightBy(by string) error {
	switch by {
	case ByApp, ByDialog:
		return nil
	}
	return newError(KindBadRequest, "a dialog is shown by %q or %q, not %q", ByApp, ByDialog, clip(by, 32))
}

// MarkInFlight records that by (ByApp or ByDialog) shows the pending
// request id's dialog now, or, with by "", that none does any more. The
// mark is in requests.json, so a broker opened after this one went away
// denies the request (Open). A request no longer pending is left alone.
func (b *Broker) MarkInFlight(id uint64, by string) error {
	if err := b.owns(); err != nil {
		return err
	}
	if by != "" {
		if err := checkInFlightBy(by); err != nil {
			return err
		}
	}
	return b.txn(func(s *state) (bool, bool, error) {
		if s.reqsErr != nil {
			return false, false, newError(KindUnavailable, "%v", s.reqsErr)
		}
		q := s.reqs.find(id)
		if q == nil {
			return false, false, newError(KindNotFound, "no request #%d in environment %s", id, b.opts.Env)
		}
		if q.State != StatePending || q.InFlight == by {
			return false, false, nil
		}
		q.InFlight = by
		return false, true, nil
	})
}

// DenyInFlight denies the pending request id because by (ByApp or
// ByDialog), which showed its dialog, went away: scope once, decided by
// by, with InFlightReason. A request no longer pending is
// ErrAlreadyAnswered.
func (b *Broker) DenyInFlight(id uint64, by string) error {
	if err := b.owns(); err != nil {
		return err
	}
	if err := checkInFlightBy(by); err != nil {
		return err
	}
	now := b.now()
	var denied *reqRec
	var expired []reqRec
	var answerErr error
	err := b.txn(func(s *state) (bool, bool, error) {
		if s.reqsErr != nil {
			return false, false, newError(KindUnavailable, "%v", s.reqsErr)
		}
		expired = expireIn(&s.reqs, now)
		wr := len(expired) > 0
		q := s.reqs.find(id)
		switch {
		case q == nil:
			answerErr = newError(KindNotFound, "no request #%d in environment %s", id, b.opts.Env)
			return false, wr, nil
		case q.State != StatePending:
			answerErr = newError(KindAlreadyAnswered, "request #%d was already answered by %s (%s)", id, q.DecidedBy, q.State)
			return false, wr, nil
		}
		s.reqs.decide(q, StateDenied, by, ScopeOnce, 0, InFlightReason(by), now)
		c := *q
		denied = &c
		return false, true, nil
	})
	for _, q := range expired {
		b.dlog.answer(now, &q, "")
	}
	if err != nil {
		return err
	}
	if answerErr != nil {
		return answerErr
	}
	b.dlog.answer(now, denied, "")
	b.logf("request #%d denied: %s", id, denied.Reason)
	return nil
}

// denyOrphans denies, as Open starts, every pending request a dialog was
// showing when the broker that marked it went away (MarkInFlight): no
// one is served before it.
func (b *Broker) denyOrphans() {
	now := b.now()
	var denied []reqRec
	err := b.txn(func(s *state) (bool, bool, error) {
		if s.reqsErr != nil {
			return false, false, nil
		}
		for i := range s.reqs.Requests {
			q := &s.reqs.Requests[i]
			if q.State != StatePending || q.InFlight == "" {
				continue
			}
			by := q.InFlight
			if checkInFlightBy(by) != nil {
				by = ByDialog
			}
			s.reqs.decide(q, StateDenied, by, ScopeOnce, 0, InFlightReason(by), now)
			denied = append(denied, *q)
		}
		return false, len(denied) > 0, nil
	})
	if err != nil {
		b.logf("cannot deny the requests whose dialog went away: %v", err)
		return
	}
	for _, q := range denied {
		b.dlog.answer(now, &q, "")
		b.logf("request #%d denied: %s", q.ID, q.Reason)
	}
	if n := len(denied); n > 0 {
		b.logf("denied %d request(s) a dialog was showing when the last broker went away", n)
	}
}
