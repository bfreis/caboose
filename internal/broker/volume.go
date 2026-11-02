package broker

import (
	"fmt"

	"github.com/bfreis/caboose/internal/policy"
)

// A volume resize a person allowed is the one request whose answer starts
// something that runs on: the Resizer runs it as a job, and what came of
// it is recorded on the request (Outcome) once it has ended, with a new
// seq, so that a sandbox waiting on its requests hears of it as of a
// decision.

// maxOutcome is the most of an outcome kept, in bytes.
const maxOutcome = 1000

// startResize starts the resize q, just allowed, asks for, and records an
// outcome at once when it does not start.
func (b *Broker) startResize(q *reqRec, res *AnswerResult) {
	id := q.ID
	r := Resize{ID: id, Volume: q.Volume, Size: q.Size, From: q.VolumeFrom, Configured: q.Configured, Profile: q.Profile}
	if err := b.opts.Resizer.ResizeVolume(r); err != nil {
		res.ResizeError = err.Error()
		b.logf("request #%d is allowed, but its resize did not start: %v", id, err)
		if err := b.RecordOutcome(id, "the resize did not start: "+err.Error()); err != nil {
			b.logf("%v", err)
		}
	}
}

// RecordOutcome records outcome, what came of an allowed volume resize,
// on request id, with a new seq; a request that is gone (pruned), or is no
// allowed resize, is left alone, and logged. The resize's job calls it
// once it has ended, through whichever broker serves the environment
// then: a link that started again meanwhile has a broker of its own.
func (b *Broker) RecordOutcome(id uint64, outcome string) error {
	if err := b.owns(); err != nil {
		return err
	}
	outcome = clip(outcome, maxOutcome)
	now := b.now()
	var done reqRec
	err := b.txn(func(s *state) (bool, bool, error) {
		if s.reqsErr != nil {
			return false, false, newError(KindUnavailable, "%v", s.reqsErr)
		}
		q := s.reqs.find(id)
		if q == nil || q.Kind != policy.KindVolumeResize || q.State != StateAllowed {
			return false, false, nil
		}
		s.reqs.Seq++
		q.Outcome, q.Seq = outcome, s.reqs.Seq
		done = *q
		return false, true, nil
	})
	if err != nil {
		return fmt.Errorf("cannot record what came of request #%d (%s): %w", id, outcome, err)
	}
	if done.ID == 0 {
		b.logf("request #%d is no longer kept as an allowed resize, so what came of it is not recorded on it: %s", id, outcome)
		return nil
	}
	b.dlog.outcome(now, &done)
	return nil
}
