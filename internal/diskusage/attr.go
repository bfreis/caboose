package diskusage

import (
	"encoding/binary"
	"fmt"
	"syscall"
)

// From <sys/attr.h>.
const (
	attrBitMapCount = 5
	// attrCmnExtPrivateSize is ATTR_CMNEXT_PRIVATESIZE, an off_t: the
	// blocks of the file no other file shares. It is in the fork
	// attributes' place, read as ATTR_CMNEXT_* under fsoptAttrCmnExtended.
	attrCmnExtPrivateSize = 0x8

	// The reply: a u_int32_t length, the attribute_set_t (five
	// u_int32_t), then the off_t.
	replySize      = 4 + 4*attrBitMapCount + 8
	returnedFork   = 4 + 4*4
	privateSizeOff = 4 + 4*attrBitMapCount
)

// parseReply reads the private size out of getattrlist's reply.
func parseReply(b []byte) (int64, error) {
	if len(b) < replySize || binary.LittleEndian.Uint32(b) < replySize {
		return 0, ErrUnsupported
	}
	if binary.LittleEndian.Uint32(b[returnedFork:])&attrCmnExtPrivateSize == 0 {
		return 0, ErrUnsupported
	}
	return int64(binary.LittleEndian.Uint64(b[privateSizeOff:])), nil
}

// callError is a failed getattrlist call's error: ErrUnsupported where
// the filesystem has no private size to give (ENOTSUP, or EINVAL for the
// extended attributes it does not know), else errno, which no errno at
// all (0, the call failing without setting it) does not hide.
func callError(call, name string, errno syscall.Errno) error {
	switch errno {
	case syscall.ENOTSUP, syscall.EINVAL:
		return ErrUnsupported
	case 0:
		return fmt.Errorf("%s %s failed with no errno", call, name)
	}
	return fmt.Errorf("%s %s: %w", call, name, errno)
}
