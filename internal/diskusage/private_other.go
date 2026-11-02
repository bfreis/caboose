//go:build !darwin

package diskusage

// DefaultUnshared is this system's UnsharedSizer: nil off macOS, where no
// filesystem caboose meets reports an unshared size.
func DefaultUnshared() UnsharedSizer { return nil }

// UnsharedMethod says how DefaultUnshared measures, for a report.
const UnsharedMethod = ""
