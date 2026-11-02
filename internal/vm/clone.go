package vm

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
)

// CloneFile makes dst a copy of src, a disk image: an APFS clone on a Mac,
// which takes no time or space until either is written, else a copy that
// keeps src's holes. dst must not exist.
func CloneFile(src, dst string) error {
	if err := clonefile(src, dst); err == nil {
		return nil
	} else if !errors.Is(err, errNoClone) {
		return err
	}
	return sparseCopy(src, dst)
}

// errNoClone is a filesystem, or a platform, that clones nothing.
var errNoClone = errors.New("no clone here")

// cloneChunk is what sparseCopy reads at a time: a chunk of zeros is a hole
// in dst.
const cloneChunk = 1 << 20

func sparseCopy(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	fi, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fi.Mode().Perm())
	if err != nil {
		return err
	}
	zero := make([]byte, cloneChunk)
	buf := make([]byte, cloneChunk)
	var off int64
	for {
		n, rerr := io.ReadFull(in, buf)
		if n > 0 && !bytes.Equal(buf[:n], zero[:n]) {
			if _, err := out.WriteAt(buf[:n], off); err != nil {
				out.Close()
				os.Remove(dst)
				return err
			}
		}
		off += int64(n)
		if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
			break
		}
		if rerr != nil {
			out.Close()
			os.Remove(dst)
			return rerr
		}
	}
	if err := out.Truncate(fi.Size()); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	return out.Close()
}

// NewDisk makes dst, which must not exist, an empty disk of size bytes: a
// clone of template, an empty ext4, made longer when size is more than
// it. The filesystem keeps template's size until the guest grows it to
// fill the disk (agent's growFS), so the host never formats or reads a
// disk. A size under template's is refused: a shorter file would cut the
// filesystem off.
func NewDisk(template, dst string, size int64) error {
	fi, err := os.Stat(template)
	if err != nil {
		return err
	}
	if size < fi.Size() {
		return fmt.Errorf("a disk of %d bytes is shorter than the empty disk it is made from, %d (%s)", size, fi.Size(), template)
	}
	if err := CloneFile(template, dst); err != nil {
		return err
	}
	if size > fi.Size() {
		if err := os.Truncate(dst, size); err != nil {
			os.Remove(dst)
			return err
		}
	}
	return nil
}
