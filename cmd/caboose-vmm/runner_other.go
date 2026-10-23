//go:build !darwin

package main

import (
	"errors"

	"github.com/bfreis/caboose/internal/vm"
	"github.com/bfreis/caboose/internal/vm/vmm"
)

// The runner off macOS: there is none, as caboose-vmm runs VMs on macOS only.
func newRunner() (vmm.Runner, error) {
	return nil, errors.New("caboose-vmm runs VMs on macOS only")
}

func check() vm.Check {
	return vm.Check{Framework: "caboose-vmm runs VMs on macOS only",
		Translated: vm.No, Supported: vm.No, Entitled: vm.Unknown, Valid: vm.Unknown}
}
