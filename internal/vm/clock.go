package vm

import (
	"context"
	"time"
)

// A guest's clock stops while the host sleeps and never catches up, and
// nothing in the guest can tell (spike 5). Only the host knows: its wall
// clock moves on through a sleep while its monotonic clock stops too. So
// the host sends its clock at every connection, whenever the two clocks
// part by more than wakeGap since the last tick (a wake, or the host's own
// clock set), and every driftEvery for the guest's drift.
const (
	ClockTick  = 10 * time.Second
	wakeGap    = time.Second
	driftEvery = time.Hour
)

// ClockKeeper keeps a guest's clock on the host's for as long as it runs:
// vmm's job, since it lives exactly as long as the VM.
type ClockKeeper struct {
	// Dial connects to the control port.
	Dial func() (*Control, error)
	// Now is the host's wall clock and a monotonic reading; nil is the
	// system's.
	Now func() (wall time.Time, mono time.Duration)
	// Tick paces the checks, and the redials; nil is every ClockTick.
	Tick <-chan time.Time
	// Logf, when set, is told what the keeper did.
	Logf func(format string, args ...any)
}

// SystemNow is ClockKeeper's default Now: the wall clock, and the
// monotonic time since the first call, which stops with the host.
func SystemNow() func() (time.Time, time.Duration) {
	start := time.Now()
	return func() (time.Time, time.Duration) {
		t := time.Now()
		return t.Round(0), t.Sub(start)
	}
}

func (k *ClockKeeper) logf(format string, args ...any) {
	if k.Logf != nil {
		k.Logf(format, args...)
	}
}

// Run keeps the clock until ctx ends, connecting again at the next tick
// whenever the connection fails or ends.
func (k *ClockKeeper) Run(ctx context.Context) {
	now := k.Now
	if now == nil {
		now = SystemNow()
	}
	tick := k.Tick
	if tick == nil {
		t := time.NewTicker(ClockTick)
		defer t.Stop()
		tick = t.C
	}
	for {
		if c, err := k.Dial(); err != nil {
			k.logf("clock: cannot reach the guest: %v", err)
		} else {
			k.keep(ctx, c, now, tick)
			c.Close()
		}
		select {
		case <-ctx.Done():
			return
		case <-tick:
		}
	}
}

// keep sends the clock on c now, and again as the host's clocks say, until
// c ends or ctx does.
func (k *ClockKeeper) keep(ctx context.Context, c *Control, now func() (time.Time, time.Duration), tick <-chan time.Time) {
	wall, mono := now()
	if err := c.Clock(wall); err != nil {
		return
	}
	sent := mono
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.Done():
			return
		case <-tick:
		}
		w, m := now()
		gap := w.Sub(wall) - (m - mono)
		wall, mono = w, m
		switch {
		case gap > wakeGap || gap < -wakeGap:
			k.logf("clock: the host's wall clock moved %v more than its monotonic one (a sleep): sending it", gap.Round(time.Millisecond))
		case m-sent >= driftEvery:
		default:
			continue
		}
		if err := c.Clock(w); err != nil {
			return
		}
		sent = m
	}
}
