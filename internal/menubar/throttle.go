package menubar

import "time"

// How many notifications the app posts on their own in a window; beyond
// that, one summary counts the rest.
const (
	ThrottleMax    = 3
	ThrottleWindow = 5 * time.Minute
)

// SummaryID is the identifier of the summary notification, which each
// new count replaces.
const SummaryID = "summary"

// Throttle decides which notifications are posted on their own: while
// fewer than ThrottleMax were in the last ThrottleWindow. The rest go into
// one summary, whose count covers what arrived since it appeared, and
// which ends once the window holds none of the posted ones.
type Throttle struct {
	posted  []time.Time
	summary int
}

// Admit takes one notification arriving at now: own says it is posted on
// its own, else summary is the summary's count, this one included.
func (t *Throttle) Admit(now time.Time) (own bool, summary int) {
	kept := t.posted[:0]
	for _, at := range t.posted {
		if now.Sub(at) < ThrottleWindow {
			kept = append(kept, at)
		}
	}
	t.posted = kept
	if len(t.posted) == 0 {
		t.summary = 0
	}
	if len(t.posted) < ThrottleMax {
		t.posted = append(t.posted, now)
		return true, 0
	}
	t.summary++
	return false, t.summary
}
