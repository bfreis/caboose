//go:build !linux

package agent

import (
	"errors"
	"io"
)

// Init and RunGuest are a vm guest's, which is linux.
func Init() int { return 1 }

func RunGuest(io.Writer) error { return errors.New("the guest agent runs only in a linux guest") }
