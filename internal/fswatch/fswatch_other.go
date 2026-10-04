//go:build !linux && !darwin

package fswatch

func start(*Watcher) (func(), error) { return nil, ErrUnsupported }
