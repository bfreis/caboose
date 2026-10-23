package broker

import (
	"encoding/json"
	"errors"
	"io/fs"
	"strconv"
	"sync"
	"time"

	"github.com/bfreis/caboose/internal/cappedlog"
	"github.com/bfreis/caboose/internal/nofollow"
	"github.com/bfreis/caboose/internal/policy"
)

// How much of what the sandbox causes decisions.log takes.
const (
	logEvery        = time.Minute
	logMax          = 120 // lines a minute of crossings
	egressEvery     = time.Hour
	maxEgressCounts = 256 // hosts counted an hour; the rest as others
)

// entry is one line of decisions.log.
type entry struct {
	Time    time.Time      `json:"time"`
	Event   string         `json:"event"` // "decision", "answer", "expiry", "revoke", "egress-edit", "egress-counts", "suppressed"
	Kind    policy.Kind    `json:"kind,omitempty"`
	Key     string         `json:"key,omitempty"`
	Verdict string         `json:"verdict,omitempty"`
	Rule    string         `json:"rule,omitempty"`
	Reason  string         `json:"reason,omitempty"`
	Request uint64         `json:"request,omitempty"`
	By      string         `json:"by,omitempty"`
	Scope   string         `json:"scope,omitempty"`
	Hours   int            `json:"hours,omitempty"`
	Grant   string         `json:"grant,omitempty"`
	Session string         `json:"session,omitempty"`
	Counts  map[string]int `json:"counts,omitempty"`
	Count   int            `json:"count,omitempty"`
}

// decisionLog is decisions.log: every decision the host should be able
// to account for, the crossings' lines limited, since the sandbox decides
// how many there are, and egress allows only as hourly counts per host.
type decisionLog struct {
	mu  sync.Mutex
	w   *cappedlog.Log // nil: not logging
	enc *json.Encoder

	from       time.Time // this period's start
	logged     int
	suppressed int

	countFrom time.Time
	counts    map[string]int
	others    int
}

// openDecisionLog opens decisions.log in dir. Anything but a plain file
// there is left alone, and nothing is logged.
func openDecisionLog(dir nofollow.Dir, envDir string, logf func(string, ...any)) *decisionLog {
	l := &decisionLog{}
	if fi, err := dir.Lstat(decisionsFile); err == nil && !fi.Mode().IsRegular() || err != nil && !errors.Is(err, fs.ErrNotExist) {
		logf("%s/%s is not a plain file: decisions are not logged", envDir, decisionsFile)
		return l
	}
	w, err := cappedlog.Open(envDir+"/"+decisionsFile, maxDecisions, "caboose broker")
	if err != nil {
		logf("decisions are not logged: %v", err)
		return l
	}
	l.w, l.enc = w, json.NewEncoder(w)
	return l
}

func (l *decisionLog) close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.w != nil {
		l.flushEgressLocked(time.Now(), true)
		l.w.Close()
		l.w = nil
	}
}

// write writes e. Under l.mu.
func (l *decisionLog) write(e entry) {
	if l.w != nil {
		_ = l.enc.Encode(e)
	}
}

// decision logs a crossing's decision: egress allows by no grant as
// counts, the rest as lines, at most logMax a period. id is the request
// it filed, if new.
func (l *decisionLog) decision(now time.Time, r policy.Request, session string, d policy.Decision, id uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.flushEgressLocked(now, false)
	if d.Verdict == policy.VerdictAllow && r.Kind == policy.KindEgress && d.Grant == "" {
		if l.counts == nil {
			l.counts = map[string]int{}
		}
		target := shown(r.Host, 100) + ":" + strconv.Itoa(r.Port)
		if _, ok := l.counts[target]; !ok && len(l.counts) >= maxEgressCounts {
			l.others++
			return
		}
		l.counts[target]++
		return
	}
	if l.from.IsZero() || now.Sub(l.from) >= logEvery || now.Before(l.from) {
		if l.suppressed > 0 {
			l.write(entry{Time: now, Event: "suppressed", Count: l.suppressed, Reason: "crossings not logged in the last period, past the limit of " + strconv.Itoa(logMax) + " lines"})
		}
		l.from, l.logged, l.suppressed = now, 0, 0
	}
	if l.logged >= logMax {
		l.suppressed++
		return
	}
	l.logged++
	verdict := map[policy.Verdict]string{policy.VerdictAllow: Allowed, policy.VerdictDeny: Denied, policy.VerdictAsk: "ask"}[d.Verdict]
	l.write(entry{Time: now, Event: "decision", Kind: policy.Kind(shown(string(r.Kind), 32)), Key: shown(policy.GrantKey(r), 600),
		Verdict: verdict, Rule: d.Rule, Reason: shown(d.Reason, 2000), Request: id, Grant: d.Grant, Session: shown(session, MaxSession)})
}

// answer logs a request's decision by a person or by expiry.
func (l *decisionLog) answer(now time.Time, q *reqRec, grant string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	ev := "answer"
	if q.DecidedBy == ByExpiry {
		ev = "expiry"
	}
	l.write(entry{Time: now, Event: ev, Kind: q.Kind, Key: shown(q.Key, 600), Verdict: q.State, Reason: shown(q.Reason, 2000),
		Request: q.ID, By: q.DecidedBy, Scope: q.Scope, Hours: q.Hours, Grant: grant, Session: shown(q.Session, MaxSession)})
}

// revoke logs a grant revoked.
func (l *decisionLog) revoke(now time.Time, g grantRec) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.write(entry{Time: now, Event: "revoke", Kind: g.Kind, Key: shown(g.Key, 600), Grant: g.ID, Request: g.Request})
}

// flushEgress writes the egress counts once an hour has passed, or now
// with force.
func (l *decisionLog) flushEgress(now time.Time, force bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.flushEgressLocked(now, force)
}

func (l *decisionLog) flushEgressLocked(now time.Time, force bool) {
	if l.countFrom.IsZero() {
		l.countFrom = now
		return
	}
	if !force && now.Sub(l.countFrom) < egressEvery && !now.Before(l.countFrom) {
		return
	}
	if len(l.counts) > 0 || l.others > 0 {
		l.write(entry{Time: now, Event: "egress-counts", Kind: policy.KindEgress, Counts: l.counts, Count: l.others, Reason: "connections allowed since " + l.countFrom.Format(time.RFC3339)})
	}
	l.countFrom, l.counts, l.others = now, nil, 0
}

// egressEdit logs a person's edit of egress_allow or egress_deny.
func (l *decisionLog) egressEdit(now time.Time, by, effect string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.write(entry{Time: now, Event: "egress-edit", Kind: policy.KindEgress, By: by, Reason: shown(effect, 2000)})
}
