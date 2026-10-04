package vm

import (
	"crypto/sha256"
	"fmt"
	"net"
	"path/filepath"
)

// MAC is the network address d's VM boots with: the same at every boot,
// so a Mac's bootpd, which leases by MAC, hands it the same lease again
// instead of a new one per boot until its pool of about 253 is used up.
// It is derived from d's absolute path, which is the VM's identity on the
// host: each environment has a data dir of its own, and in it the sandbox
// and the builder each a directory of their own, so no two VMs that can
// share a Mac's NAT share a MAC, but by a hash's chance.
func (d Dir) MAC() string {
	p, err := filepath.Abs(string(d))
	if err != nil {
		p = string(d)
	}
	return MACFor(p)
}

// MACFor is id's MAC: a locally administered unicast address (the first
// octet's bit 1 set, bit 0 clear), its other 46 bits from id's sha256.
func MACFor(id string) string {
	h := sha256.Sum256([]byte("caboose vm mac\x00" + id))
	h[0] = h[0]&^0x01 | 0x02
	return net.HardwareAddr(h[:6]).String()
}

// CheckMAC says what is wrong with mac as a VM's address, which must be a
// locally administered unicast Ethernet address, written as MACFor writes
// one.
func CheckMAC(mac string) error {
	hw, err := net.ParseMAC(mac)
	switch {
	case err != nil || len(hw) != 6 || hw.String() != mac:
		return fmt.Errorf("the MAC %q is not an Ethernet address as xx:xx:xx:xx:xx:xx", mac)
	case hw[0]&0x01 != 0:
		return fmt.Errorf("the MAC %s is multicast", mac)
	case hw[0]&0x02 == 0:
		return fmt.Errorf("the MAC %s is not locally administered", mac)
	}
	return nil
}
