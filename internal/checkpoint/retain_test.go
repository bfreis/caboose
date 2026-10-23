package checkpoint

import (
	"fmt"
	"math/rand"
	"sort"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)

func ago(d time.Duration) time.Time { return t0.Add(-d) }

func contains(ts []time.Time, s time.Time) bool {
	for _, x := range ts {
		if x.Equal(s) {
			return true
		}
	}
	return false
}

func TestRetainBoundaries(t *testing.T) {
	s := time.Second
	cases := []struct {
		name   string
		stamps []time.Time
		kept   []time.Time
	}{
		{"just under 2h", []time.Time{ago(2*time.Hour - s), ago(2*time.Hour - 2*s)}, []time.Time{ago(2*time.Hour - 2*s), ago(2*time.Hour - s)}},
		// At exactly 2h the hour rule applies: 10:00:00 gives way to the
		// newer 10:00:01 of its hour; 09:59:59 is another hour's newest.
		{"exactly 2h", []time.Time{ago(2 * time.Hour), ago(2*time.Hour + s), ago(2*time.Hour - s)}, []time.Time{ago(2*time.Hour - s), ago(2*time.Hour + s)}},
		{"exactly 2h alone in its hour", []time.Time{ago(2 * time.Hour), ago(2*time.Hour + 30*time.Minute)}, []time.Time{ago(2 * time.Hour), ago(2*time.Hour + 30*time.Minute)}},
		{"same hour past 2h", []time.Time{ago(2*time.Hour + 10*time.Minute), ago(2*time.Hour + 20*time.Minute), ago(2*time.Hour + 59*time.Minute)}, []time.Time{ago(2*time.Hour + 10*time.Minute)}},
		{"hour edge", []time.Time{ago(3 * time.Hour), ago(3*time.Hour + s)}, []time.Time{ago(3 * time.Hour), ago(3*time.Hour + s)}},
		{"just under 48h", []time.Time{ago(48*time.Hour - s), ago(48*time.Hour - 2*s)}, []time.Time{ago(48*time.Hour - 2*s)}},
		// 48h ago is 12:00 two days back: the day band starts there.
		{"exactly 48h", []time.Time{ago(48 * time.Hour), ago(48*time.Hour + time.Hour)}, []time.Time{ago(48 * time.Hour)}},
		{"48h across hour and day bands", []time.Time{ago(48*time.Hour - s), ago(48 * time.Hour)}, []time.Time{ago(48*time.Hour - s)}},
		{"day edge", []time.Time{ago(60 * time.Hour), ago(60*time.Hour + s)}, []time.Time{ago(60 * time.Hour), ago(60*time.Hour + s)}},
		{"same day", []time.Time{ago(72 * time.Hour), ago(80 * time.Hour)}, []time.Time{ago(72 * time.Hour)}},
		{"just under 7d", []time.Time{ago(7*24*time.Hour - s)}, []time.Time{ago(7*24*time.Hour - s)}},
		{"exactly 7d", []time.Time{ago(7 * 24 * time.Hour)}, nil},
		{"past 7d", []time.Time{ago(7*24*time.Hour + s), ago(30 * 24 * time.Hour)}, nil},
		{"negative age", []time.Time{t0.Add(time.Hour), t0.Add(time.Minute)}, []time.Time{t0.Add(time.Hour), t0.Add(time.Minute)}},
		{"duplicates", []time.Time{ago(time.Minute), ago(time.Minute), ago(8 * 24 * time.Hour), ago(8 * 24 * time.Hour)}, []time.Time{ago(time.Minute)}},
		{"empty", nil, nil},
	}
	for _, c := range cases {
		keep, drop := Retain(c.stamps, Rules{From: t0}, nil)
		if fmt.Sprint(keep) != fmt.Sprint(c.kept) {
			t.Errorf("%s: kept %v, want %v", c.name, keep, c.kept)
		}
		for _, d := range drop {
			if contains(keep, d) {
				t.Errorf("%s: %v both kept and dropped", c.name, d)
			}
		}
	}
	// Local times are bucketed by their UTC hour.
	z := time.FixedZone("x", 30*60)
	keep, _ := Retain([]time.Time{ago(3*time.Hour + 5*time.Minute).In(z), ago(3*time.Hour - 5*time.Minute).In(z)}, Rules{From: t0}, nil)
	if len(keep) != 2 {
		t.Errorf("UTC hour buckets: kept %v", keep)
	}
	// Pinned and KeepNewest.
	old := ago(14 * 24 * time.Hour)
	keep, _ = Retain([]time.Time{old, ago(15 * 24 * time.Hour)}, Rules{From: t0}, []time.Time{ago(15 * 24 * time.Hour)})
	if len(keep) != 1 || !keep[0].Equal(ago(15*24*time.Hour)) {
		t.Errorf("pinned: kept %v", keep)
	}
	keep, _ = Retain([]time.Time{old}, DefaultRules(t0), []time.Time{old})
	if !contains(keep, old) {
		t.Error("pinned dropped")
	}
	keep, _ = Retain([]time.Time{old, ago(15 * 24 * time.Hour)}, Rules{From: t0, KeepNewest: true}, nil)
	if len(keep) != 1 || !keep[0].Equal(old) {
		t.Errorf("KeepNewest kept %v", keep)
	}
	// From the newest stamp: a long stop does not count.
	keep, _ = Retain([]time.Time{old, old.Add(-time.Hour)}, Rules{From: old}, nil)
	if len(keep) != 2 {
		t.Errorf("From newest kept %v", keep)
	}
}

// TestRetainDefault is the rule callers use: the newest is kept however
// old, and ages count from it, so a long stop changes nothing.
func TestRetainDefault(t *testing.T) {
	h := time.Hour
	// The last checkpoints before a stop of 30 days: every 10 minutes for
	// three days.
	stop := ago(30 * 24 * h)
	var ss []time.Time
	for d := time.Duration(0); d < 72*h; d += 10 * time.Minute {
		ss = append(ss, stop.Add(-d))
	}
	atStop, _ := Retain(ss, Rules{From: stop}, nil)
	keep, drop := Retain(ss, DefaultRules(t0), nil)
	if fmt.Sprint(keep) != fmt.Sprint(atStop) {
		t.Errorf("after a 30-day stop kept %v, want what was kept at the stop, %v", keep, atStop)
	}
	if len(keep) == 0 || !keep[0].Equal(stop) || len(drop) == 0 {
		t.Errorf("kept %v, dropped %d", keep, len(drop))
	}
	// The wall clock would have dropped them all.
	if k, _ := Retain(ss, Rules{From: t0}, nil); len(k) != 0 {
		t.Errorf("from now kept %v", k)
	}
	// A lone checkpoint of a year ago is kept.
	year := ago(365 * 24 * h)
	if k, _ := Retain([]time.Time{year}, DefaultRules(t0), nil); len(k) != 1 {
		t.Errorf("the newest, a year old: kept %v", k)
	}
	// Ages from the newest: one 7 days before it goes, one just under is
	// kept, whatever now is.
	n := ago(100 * 24 * h)
	k, _ := Retain([]time.Time{n, n.Add(-7 * 24 * h), n.Add(-7*24*h + time.Second)}, DefaultRules(t0), nil)
	if fmt.Sprint(k) != fmt.Sprint([]time.Time{n, n.Add(-7*24*h + time.Second)}) {
		t.Errorf("ages from the newest: kept %v", k)
	}
	// A newest stamped ahead of now (the clock set back): ages are from
	// now, so what is taken now is young, and the one ahead is kept.
	ahead := t0.Add(400 * 24 * h)
	k, _ = Retain([]time.Time{ahead, ago(time.Minute), ago(10 * 24 * h)}, DefaultRules(t0), nil)
	if fmt.Sprint(k) != fmt.Sprint([]time.Time{ahead, ago(time.Minute)}) {
		t.Errorf("a newest ahead of now: kept %v", k)
	}
}

func TestRetainProperties(t *testing.T) {
	r := rand.New(rand.NewSource(42))
	const n = 10000
	for _, keepNewest := range []bool{false, true} {
		for _, from := range []string{"now", "newest", "earlier", "fromNewest"} {
			t.Run(fmt.Sprintf("keepNewest=%v/from=%s", keepNewest, from), func(t *testing.T) {
				for i := 0; i < n; i++ {
					checkRetain(t, r, keepNewest, from)
				}
			})
		}
	}
}

func randomStamps(r *rand.Rand) []time.Time {
	var ss []time.Time
	k := r.Intn(40)
	for j := 0; j < k; j++ {
		var d time.Duration
		switch r.Intn(4) {
		case 0:
			d = time.Duration(r.Int63n(int64(3 * time.Hour)))
		case 1:
			d = time.Duration(r.Int63n(int64(50 * time.Hour)))
		case 2:
			d = time.Duration(r.Int63n(int64(9 * 24 * time.Hour)))
		default:
			d = time.Duration(r.Int63n(int64(20*24*time.Hour))) - 24*time.Hour
		}
		s := t0.Add(-d).Truncate(time.Second)
		ss = append(ss, s)
		if r.Intn(10) == 0 {
			ss = append(ss, s) // a duplicate
		}
	}
	return ss
}

func newest(ss []time.Time) time.Time {
	var n time.Time
	for _, s := range ss {
		if s.After(n) {
			n = s
		}
	}
	return n
}

func rulesFor(r *rand.Rand, ss []time.Time, keepNewest bool, from string) Rules {
	rr := Rules{KeepNewest: keepNewest, From: t0}
	switch from {
	case "newest":
		rr.From = newest(ss)
	case "fromNewest":
		rr.FromNewest = true
	case "earlier":
		if len(ss) > 0 {
			rr.From = ss[r.Intn(len(ss))].Add(-time.Duration(r.Int63n(int64(time.Hour))))
		}
	}
	return rr
}

func checkRetain(t *testing.T, r *rand.Rand, keepNewest bool, from string) {
	t.Helper()
	ss := randomStamps(r)
	var pinned []time.Time
	for _, s := range ss {
		if r.Intn(15) == 0 {
			pinned = append(pinned, s)
		}
	}
	rr := rulesFor(r, ss, keepNewest, from)
	keep, drop := Retain(ss, rr, pinned)
	fail := func(f string, a ...any) {
		t.Helper()
		t.Fatalf("stamps %v rules %+v pinned %v: keep %v drop %v: %s", ss, rr, pinned, keep, drop, fmt.Sprintf(f, a...))
	}
	// keep ∪ drop = input, disjoint, newest first.
	uniq := map[int64]bool{}
	for _, s := range ss {
		uniq[s.UnixNano()] = true
	}
	if len(keep)+len(drop) != len(uniq) {
		fail("sizes")
	}
	for _, s := range append(append([]time.Time{}, keep...), drop...) {
		if !uniq[s.UnixNano()] {
			fail("%v not in input", s)
		}
	}
	for _, s := range keep {
		if contains(drop, s) {
			fail("%v in both", s)
		}
	}
	for _, l := range [][]time.Time{keep, drop} {
		if !sort.SliceIsSorted(l, func(i, j int) bool { return l[i].After(l[j]) }) {
			fail("not newest first")
		}
	}
	isPinned := func(s time.Time) bool { return contains(pinned, s) }
	isNewest := func(s time.Time) bool { return keepNewest && s.Equal(newest(ss)) }
	from0 := rr.From
	if rr.FromNewest && len(ss) > 0 && newest(ss).Before(from0) {
		from0 = newest(ss)
	}
	age := func(s time.Time) time.Duration { return max(from0.Sub(s), 0) }
	for _, s := range drop {
		if age(s) < 2*time.Hour {
			fail("dropped %v younger than 2h", s)
		}
		if isPinned(s) {
			fail("dropped pinned %v", s)
		}
		if isNewest(s) {
			fail("dropped the newest %v", s)
		}
	}
	hours, days := map[int64]int{}, map[int64]int{}
	for _, s := range keep {
		if isPinned(s) || isNewest(s) {
			continue
		}
		a := age(s)
		switch {
		case a < 2*time.Hour:
		case a < 48*time.Hour:
			hours[s.UTC().Truncate(time.Hour).Unix()]++
		case a < 7*24*time.Hour:
			days[s.UTC().Truncate(24*time.Hour).Unix()]++
		default:
			fail("kept %v, 7 days old or more", s)
		}
	}
	for b, c := range hours {
		if c > 1 {
			fail("%d kept in hour %v", c, time.Unix(b, 0).UTC())
		}
	}
	for b, c := range days {
		if c > 1 {
			fail("%d kept in day %v", c, time.Unix(b, 0).UTC())
		}
	}
	// Idempotent.
	if _, d := Retain(keep, rr, pinned); len(d) != 0 {
		fail("Retain of the kept set drops %v", d)
	}
	// A stamp newer than all never resurrects a dropped one.
	if len(ss) > 0 {
		nw := newest(ss).Add(time.Duration(1+r.Intn(3*3600)) * time.Second)
		rr2 := rr
		if from == "newest" {
			rr2.From = nw
		}
		keep2, _ := Retain(append(append([]time.Time{}, ss...), nw), rr2, pinned)
		for _, s := range drop {
			if contains(keep2, s) {
				fail("adding %v resurrects %v", nw, s)
			}
		}
	}
}
