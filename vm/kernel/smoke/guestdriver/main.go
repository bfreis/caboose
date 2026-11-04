// Command guestdriver is the launcher's side of a vm guest's boot in make
// vm-guest-smoke's stage b: the production vm.Dir and vm.Control, over the
// hybrid vsock socket DIR/vm.sock that vhost-device-vsock serves for QEMU.
// It prints "DRIVER READY" once the guest says it is ready, "DRIVER SHUTDOWN
// ok" once it was asked to stop, and "DRIVER FAIL ..." on anything else, in
// which case it exits 1. Nothing it does waits past -timeout.
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/bfreis/caboose/internal/agentproto"
	"github.com/bfreis/caboose/internal/vm"
)

func main() {
	dir := flag.String("dir", "", "the directory of vm.sock")
	timeout := flag.Duration("timeout", time.Minute, "how long the whole boot may take")
	noReady := flag.Bool("no-ready", false, "boot an entrypoint that never makes the ready file")
	flag.Parse()

	cmd := []string{"/guestboot"}
	if *noReady {
		cmd = append(cmd, "no-ready")
	}
	spec := agentproto.BootSpec{
		Hostname: "guest-smoke",
		User:     "0:0",
		Cmd:      cmd,
		Ready:    "/run/ready",
		Env:      []string{"HOME=/", "PATH=/bin"},
	}
	start := time.Now()
	if err := run(vm.Dir(*dir), spec, start.Add(*timeout)); err != nil {
		fmt.Printf("DRIVER FAIL after %v: %v\n", time.Since(start).Round(time.Millisecond), err)
		os.Exit(1)
	}
}

func run(d vm.Dir, spec agentproto.BootSpec, deadline time.Time) error {
	if err := awaitAgent(d, deadline); err != nil {
		return err
	}
	if err := d.WaitReady(spec, time.Until(deadline)); err != nil {
		return err
	}
	fmt.Println("DRIVER READY")
	c, err := d.Control(time.Until(deadline))
	if err != nil {
		return err
	}
	defer c.Close()
	if err := c.Shutdown(); err != nil {
		return err
	}
	fmt.Println("DRIVER SHUTDOWN ok")
	return nil
}

// awaitAgent waits for the agent to listen on the control port. vm.Dir.Control
// waits only on hvsock.ErrRefused, caboose-vmm's answer while nothing listens:
// vhost-device-vsock closes the connection before reading the CONNECT, a
// failed write, or has no socket yet at all. Any failure is the same here.
func awaitAgent(d vm.Dir, deadline time.Time) error {
	for {
		c, err := vm.DialControl(d.Socket(), 5*time.Second)
		if err == nil {
			return c.Close()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the agent never listened: %w", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
