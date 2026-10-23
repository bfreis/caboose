package launcher

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// fsTypeName names the filesystem of p, for the types checkpoints care
// about; any other is its magic number.
func fsTypeName(p string) (string, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(p, &st); err != nil {
		return "", err
	}
	switch magic := uint32(st.Type); magic {
	case 0x9123683E:
		return "btrfs", nil
	case 0x58465342:
		return "xfs", nil
	case 0x2FC12FC1:
		return "zfs", nil
	case 0xCA451A4E:
		return "bcachefs", nil
	case 0xEF53:
		return "ext4", nil
	case 0x01021994:
		return "tmpfs", nil
	default:
		return fmt.Sprintf("0x%x", magic), nil
	}
}
