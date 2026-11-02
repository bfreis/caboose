package vm

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/bfreis/caboose/internal/hvsock"
)

// vmm's own port on vm.sock: a CONNECT to VMMPort reaches caboose-vmm
// itself, never the guest, which listens on no port that high. On it the
// launcher asks about, and changes, the size of the VM's BlockDevices: one
// request per connection, a line of JSON (DiskRequest), answered by a line
// of JSON (DiskAnswer). An older caboose-vmm passes the port on to the
// guest, where nothing listens: the CONNECT is refused (ErrOldVMM).
const VMMPort = 0xfffffffe

// maxDiskLine bounds a request's line and an answer's, either way.
const maxDiskLine = 4096

// The requests vmm takes on VMMPort.
const (
	// DiskSize asks for a block device's size: the capacity it serves.
	DiskSize = "size"
	// DiskResize makes a block device Size bytes, the guest told
	// (Runner.ResizeDisk).
	DiskResize = "resize"
)

// DiskRequest is a request on VMMPort, for the block device whose serial
// is Serial (a volume's name).
type DiskRequest struct {
	Op     string `json:"op"`
	Serial string `json:"serial"`
	Size   int64  `json:"size,omitempty"`
}

// DiskAnswer is vmm's answer. Size is the device's capacity after the
// request. For a resize, Applied says whether the disk has the new size
// (vz.ResizeApplied: the size a caller records), Told whether the guest
// confirmed it, and FileSize, when not 0, that the disk's file was left at
// another size than the disk (a vz.FileError: FileError says why). Error
// is why the request failed, alongside what it left.
type DiskAnswer struct {
	Size      int64  `json:"size"`
	Applied   bool   `json:"applied,omitempty"`
	Told      bool   `json:"told,omitempty"`
	FileSize  int64  `json:"file_size,omitempty"`
	FileError string `json:"file_error,omitempty"`
	Error     string `json:"error,omitempty"`
}

// ErrOldVMM is a caboose-vmm that takes no requests of its own: older
// than VMMPort.
var ErrOldVMM = errors.New("this VM's caboose-vmm takes no disk requests: it is older than this caboose")

// AskDisks sends req to vmm behind socket and returns its answer, all
// within timeout.
func AskDisks(socket string, req DiskRequest, timeout time.Duration) (DiskAnswer, error) {
	c, err := hvsock.Dial(socket, VMMPort, timeout)
	if errors.Is(err, hvsock.ErrRefused) {
		return DiskAnswer{}, ErrOldVMM
	}
	if err != nil {
		return DiskAnswer{}, err
	}
	defer c.Close()
	if err := c.SetDeadline(time.Now().Add(timeout)); err != nil {
		return DiskAnswer{}, err
	}
	b, _ := json.Marshal(req)
	if _, err := c.Write(append(b, '\n')); err != nil {
		return DiskAnswer{}, err
	}
	var a DiskAnswer
	if err := readJSONLine(c, &a); err != nil {
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			return DiskAnswer{}, fmt.Errorf("caboose-vmm did not answer in %v", timeout)
		}
		return DiskAnswer{}, fmt.Errorf("caboose-vmm's answer: %w", err)
	}
	return a, nil
}

// ServeDisks answers one request on c, a connection to VMMPort, with do,
// reading it within timeout.
func ServeDisks(c io.ReadWriter, setDeadline func(time.Time) error, timeout time.Duration, do func(DiskRequest) DiskAnswer) error {
	if err := setDeadline(time.Now().Add(timeout)); err != nil {
		return err
	}
	var req DiskRequest
	if err := readJSONLine(c, &req); err != nil {
		return err
	}
	if err := setDeadline(time.Time{}); err != nil {
		return err
	}
	b, _ := json.Marshal(do(req))
	_, err := c.Write(append(b, '\n'))
	return err
}

// readJSONLine reads one line of at most maxDiskLine bytes into v.
func readJSONLine(r io.Reader, v any) error {
	line, err := bufio.NewReaderSize(io.LimitReader(r, maxDiskLine+1), maxDiskLine+1).ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) || len(line) > maxDiskLine {
		return errors.New("a line too long")
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(line, v)
}
