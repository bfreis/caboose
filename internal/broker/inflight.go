package broker

// ReasonDialogGone is the reason a request in flight is denied with when
// the dialog that showed it went away: never asked again another way,
// since the person may have been about to deny it, and a second dialog
// trains click-through.
const ReasonDialogGone = "the dialog went away; ask again"

// MarkInFlight records that by (ByDialog) shows the pending request id's
// dialog now, or, with by "", that none does any more. The mark is in
// requests.json, so a broker opened after this one went away denies the
// request (Open). A request no longer pending is left alone.
func (b *Broker) MarkInFlight(id uint64, by string) error {
	if err := b.owns(); err != nil {
		return err
	}
	if by != "" && by != ByDialog {
		return newError(KindBadRequest, "a dialog is shown by %q, not %q", ByDialog, clip(by, 32))
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
			s.reqs.decide(q, StateDenied, ByDialog, ScopeOnce, 0, ReasonDialogGone, now)
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
