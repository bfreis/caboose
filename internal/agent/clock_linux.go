package agent

import (
	"time"

	"golang.org/x/sys/unix"
)

// SystemClock is the guest kernel's realtime clock; setting it needs
// CAP_SYS_TIME, which the init has. Every container on the guest shares
// it.
type SystemClock struct{}

func (SystemClock) Now() time.Time { return time.Now() }

func (SystemClock) Step(to time.Time) error {
	ts := unix.NsecToTimespec(to.UnixNano())
	return unix.ClockSettime(unix.CLOCK_REALTIME, &ts)
}

// Slew is adjtime(3)'s: the kernel runs the clock up to 0.05% fast or slow
// until d is made up.
func (SystemClock) Slew(d time.Duration) error {
	tx := unix.Timex{Modes: unix.ADJ_OFFSET_SINGLESHOT, Offset: d.Microseconds()}
	_, err := unix.Adjtimex(&tx)
	return err
}
