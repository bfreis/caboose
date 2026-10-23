package macui

import "github.com/bfreis/caboose/internal/menubar"

// alertKind is what an alert answers.
type alertKind int

const (
	alertConfirm alertKind = iota // answers through Events.Confirmed
	alertURL                      // answers through Events.URLAnswered
	alertInfo                     // only OK: its closing through Events.AlertClosed
)

// alertReq is one alert to show.
type alertReq struct {
	kind alertKind
	id   string
	text alertText
	// gen tells this showing apart from any other, for the timer that
	// enables its action button.
	gen uint64
	// dismissed is set when Dismiss or Quit closed it: its result is not
	// the user's, and nothing is posted.
	dismissed bool
}

// confirmReq, urlReq and infoReq are the alerts for a confirmation, a URL
// dialog and an informational alert.
func confirmReq(c menubar.Confirmation) *alertReq {
	return &alertReq{kind: alertConfirm, id: c.ID, text: confirmText(c)}
}

func urlReq(d menubar.URLDialog) *alertReq {
	return &alertReq{kind: alertURL, id: d.ID, text: urlText(d)}
}

func infoReq(id, text string) *alertReq {
	return &alertReq{kind: alertInfo, id: id, text: infoText(text)}
}

// alertQueue holds the alerts: one shown at a time, the rest waiting in
// the order they came. It is only ever used on the main thread.
type alertQueue struct {
	pending  []*alertReq
	cur      *alertReq
	gen      uint64
	quitting bool
}

// push queues an alert.
func (q *alertQueue) push(r *alertReq) {
	if q.quitting {
		return
	}
	q.pending = append(q.pending, r)
}

// next takes the alert to show now, or nil while one is shown, none
// waits, or the app is quitting.
func (q *alertQueue) next() *alertReq {
	if q.cur != nil || q.quitting || len(q.pending) == 0 {
		return nil
	}
	r := q.pending[0]
	q.pending = q.pending[1:]
	q.gen++
	r.gen = q.gen
	q.cur = r
	return r
}

// showing reports whether the alert of generation gen is the one shown.
func (q *alertQueue) showing(gen uint64) bool {
	return q.cur != nil && q.cur.gen == gen
}

// enableIfShowing runs enable, which enables an alert's action button,
// only while the alert of generation gen is still the one shown: a timer
// left from an earlier alert never touches a later one's button.
func (q *alertQueue) enableIfShowing(gen uint64, enable func()) {
	if q.showing(gen) {
		enable()
	}
}

// dismissable reports whether Dismiss(id) closes r: only a confirmation
// or a URL dialog, named by its own non-empty id.
func dismissable(r *alertReq, id string) bool {
	return id != "" && r.id == id && r.kind != alertInfo
}

// dismiss drops every waiting alert with this id, and reports whether
// the one shown has it, which the caller then aborts. Neither posts an
// answer. An empty id, or an informational alert's, closes nothing.
func (q *alertQueue) dismiss(id string) (abort bool) {
	keep := q.pending[:0]
	for _, r := range q.pending {
		if !dismissable(r, id) {
			keep = append(keep, r)
		}
	}
	clear(q.pending[len(keep):])
	q.pending = keep
	if q.cur != nil && dismissable(q.cur, id) && !q.cur.dismissed {
		q.cur.dismissed = true
		return true
	}
	return false
}

// quit drops every waiting alert and reports whether one is shown, which
// the caller then aborts; no alert is shown after.
func (q *alertQueue) quit() (abort bool) {
	q.quitting = true
	clear(q.pending)
	q.pending = nil
	if q.cur != nil {
		q.cur.dismissed = true
		return true
	}
	return false
}

// done ends the alert shown with runModal's result, and posts its answer,
// or an informational alert's closing, unless it was dismissed.
func (q *alertQueue) done(code int, ev menubar.Events) {
	r := q.cur
	q.cur = nil
	if r == nil || r.dismissed || ev == nil {
		return
	}
	switch r.kind {
	case alertConfirm:
		ev.Confirmed(r.id, approved(code))
	case alertURL:
		ev.URLAnswered(r.id, approved(code))
	case alertInfo:
		ev.AlertClosed(r.id) // whatever closed it: there is only OK
	}
}
