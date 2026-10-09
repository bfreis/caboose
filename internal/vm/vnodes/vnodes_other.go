//go:build !darwin

package vnodes

// Host is this machine, which has no Virtualization processes.
func Host() (System, error) { return nil, ErrUnsupported }
