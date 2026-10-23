package checkpoint

import "golang.org/x/sys/unix"

// statTimes is st's access, modification and change times.
func statTimes(st *unix.Stat_t) (atime, mtime, ctime unix.Timespec) {
	return st.Atim, st.Mtim, st.Ctim
}
