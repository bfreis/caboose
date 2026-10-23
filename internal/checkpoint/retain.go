package checkpoint

import (
	"sort"
	"time"
)

// The retention bands, by age.
const (
	keepAll   = 2 * time.Hour      // younger: every checkpoint kept
	keepHour  = 48 * time.Hour     // younger: the newest of each UTC hour
	keepDay   = 7 * 24 * time.Hour // younger: the newest of each UTC day
	dayLength = 24 * time.Hour
)

// Rules are the parameters of Retain.
//
// The default (DefaultRules) always keeps the newest checkpoint and
// measures ages from it (FromNewest), not from the clock: time in which
// none was taken, an environment left stopped for a month, does not count,
// so what was kept when the last one was taken is what is kept after.
type Rules struct {
	KeepNewest bool      // keep the newest checkpoint whatever its age
	From       time.Time // the instant ages are measured from, at the latest
	// FromNewest measures ages from the newest stamp instead of From when
	// it is earlier. A newest stamp after From (the clock was set back) is
	// not used: ages are then from From, so checkpoints taken now are not
	// judged old against one stamped ahead of them.
	FromNewest bool
}

// DefaultRules are the rules callers use: the newest always kept, and
// ages from it, or from now when it is stamped after now.
func DefaultRules(now time.Time) Rules { return Rules{KeepNewest: true, From: now, FromNewest: true} }

// Retain decides which of a key's checkpoints (by stamp) to keep. With
// age = from - stamp, from being r.From, or the newest stamp when
// r.FromNewest and it is earlier (a negative age, a clock set back, counts as 0):
// younger than 2 hours, kept; from 2 to 48 hours, kept if it is the newest
// of its UTC hour; from 48 hours to 7 days, kept if it is the newest of its
// UTC day; 7 days or older, dropped. A stamp in pinned is always kept, and
// with r.KeepNewest so is the newest. Equal stamps count once. Both results
// are newest first.
//
// "The newest of its hour (day)" is among all the stamps, younger ones
// included, not only those in the same band: so a stamp dropped stays
// dropped as ages grow (From later) or newer stamps come, which a rule
// judging each band alone would not give. A stamp of an hour that is
// partly younger than 2 hours is dropped once a newer one of the same hour
// exists, which is then that hour's checkpoint (and, later, its day's).
func Retain(stamps []time.Time, r Rules, pinned []time.Time) (keep, drop []time.Time) {
	uniq := make(map[int64]time.Time, len(stamps))
	for _, s := range stamps {
		uniq[s.UnixNano()] = s
	}
	all := make([]time.Time, 0, len(uniq))
	for _, s := range uniq {
		all = append(all, s)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].After(all[j]) })
	pin := make(map[int64]bool, len(pinned))
	for _, p := range pinned {
		pin[p.UnixNano()] = true
	}
	// Newest first, so the first one seen in a bucket is its newest.
	from := r.From
	if r.FromNewest && len(all) > 0 && all[0].Before(from) {
		from = all[0]
	}
	hours := map[int64]bool{}
	days := map[int64]bool{}
	for i, s := range all {
		hour := s.UTC().Truncate(time.Hour).Unix()
		day := s.UTC().Truncate(dayLength).Unix()
		newestOfHour, newestOfDay := !hours[hour], !days[day]
		hours[hour], days[day] = true, true
		age := max(from.Sub(s), 0)
		kept := false
		switch {
		case age < keepAll:
			kept = true
		case age < keepHour:
			kept = newestOfHour
		case age < keepDay:
			kept = newestOfDay
		}
		if pin[s.UnixNano()] || r.KeepNewest && i == 0 {
			kept = true
		}
		if kept {
			keep = append(keep, s)
		} else {
			drop = append(drop, s)
		}
	}
	return keep, drop
}
