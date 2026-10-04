//go:build !darwin

package main

import (
	"errors"

	"github.com/bfreis/caboose/internal/vm"
	"github.com/bfreis/caboose/internal/vm/vmm"
)

// Cloud Hypervisor, on Linux, is step 4's.
func newRunner() (vmm.Runner, error) {
	return nil, errors.New("caboose-vmm runs VMs on macOS only, for now")
}

func check() vm.Check {
	return vm.Check{Framework: "caboose-vmm runs VMs on macOS only, for now",
		Translated: vm.No, Supported: vm.No, Entitled: vm.Unknown, Valid: vm.Unknown}
}
