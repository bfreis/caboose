package vmm

import (
	"io"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/hvsock"
)

// pairRunner hands each connection's guest end to the benchmark: a
// non-blocking socket in an *os.File, as vz gives vmm its end.
type pairRunner struct {
	Runner
	guests chan *os.File
}

func (r *pairRunner) Connect(uint32) (io.ReadWriteCloser, error) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	for _, fd := range fds {
		if err := syscall.SetNonblock(fd, true); err != nil {
			return nil, err
		}
	}
	r.guests <- os.NewFile(uintptr(fds[1]), "guest")
	return os.NewFile(uintptr(fds[0]), "vsock"), nil
}

// BenchmarkRelay moves 64 MiB one way through vm.sock and vmm's splice.
func BenchmarkRelay(b *testing.B) {
	for _, toGuest := range []bool{true, false} {
		name := map[bool]string{true: "host-to-guest", false: "guest-to-host"}[toGuest]
		b.Run(name, func(b *testing.B) {
			sock := filepath.Join(b.TempDir(), "vm.sock")
			ln, err := net.Listen("unix", sock)
			if err != nil {
				b.Fatal(err)
			}
			defer ln.Close()
			r := &pairRunner{guests: make(chan *os.File, 1)}
			go serve(ln, r)
			const total = 64 << 20
			b.SetBytes(total)
			for i := 0; i < b.N; i++ {
				c, err := hvsock.Dial(sock, 1026, 5*time.Second)
				if err != nil {
					b.Fatal(err)
				}
				g := <-r.guests
				var src io.Writer = c
				var dst io.Reader = g
				if !toGuest {
					src, dst = g, c
				}
				done := make(chan struct{})
				go func() {
					_, _ = io.Copy(io.Discard, dst)
					close(done)
				}()
				buf := make([]byte, 64<<10)
				for sent := 0; sent < total; sent += len(buf) {
					if _, err := src.Write(buf); err != nil {
						b.Fatal(err)
					}
				}
				c.Close()
				g.Close()
				<-done
			}
		})
	}
}
