// Package e2e boots a real caboose VM on a Mac: the launcher's backend.VM,
// a signed caboose-vmm on Virtualization.framework, the agent's init and
// servers in the guest. It needs the files CABOOSE_VM_E2E names, so it is
// skipped everywhere else; build it with go test -c and run it on a Mac:
//
//	CABOOSE_VM_E2E=DIR ./e2e.test -test.v
//
// DIR holds caboose-vmm (signed), Image (the kernel),
// caboose-agent-linux-arm64, root.img (an ext4 root with /bin/sh) and
// empty-ext4.img, the 4G empty ext4 every disk is made from (longer, the
// guest growing its filesystem): a copy of the builder's, which a data dir
// keeps as vm/images/empty-ext4-4g.img.
package e2e
