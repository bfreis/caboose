package main

import (
	"github.com/bfreis/caboose/internal/vm"
	"github.com/bfreis/caboose/internal/vm/vmm"
	"github.com/bfreis/caboose/internal/vm/vz"
)

func newRunner() (vmm.Runner, error) { return vz.New() }

func check() vm.Check { return vz.Check() }
