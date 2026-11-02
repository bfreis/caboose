// Command stallprobe is TestVolumeFreezeStall's writer in the guest: it
// writes 4 KiB to a file (buffered, one write(2) each) every 2 ms for a
// while, wrapping at 64 MiB, and prints how long the writes took as JSON.
// A write that waits on a frozen filesystem is a stall. Built for the
// guest into the e2e's directory:
//
//	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o DIR/stallprobe-linux-arm64 ./internal/vm/e2e/stallprobe
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"
)

func main() {
	file := flag.String("file", "", "the file to write")
	run := flag.Duration("for", 30*time.Second, "how long to write")
	flag.Parse()
	f, err := os.OpenFile(*file, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	buf := make([]byte, 4096)
	var res struct {
		Writes  int     `json:"writes"`
		MaxMS   float64 `json:"max_ms"`
		MeanMS  float64 `json:"mean_ms"`
		Over10  int     `json:"over_10ms"`
		Over100 int     `json:"over_100ms"`
		// MaxAt is when the slowest write began, as Unix milliseconds.
		MaxAt int64 `json:"max_at_unix_ms"`
	}
	var total time.Duration
	var off int64
	for end := time.Now().Add(*run); time.Now().Before(end); time.Sleep(2 * time.Millisecond) {
		t0 := time.Now()
		if _, err := f.WriteAt(buf, off); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		d := time.Since(t0)
		if off += int64(len(buf)); off >= 64<<20 {
			off = 0
		}
		res.Writes++
		total += d
		if ms := float64(d) / 1e6; ms > res.MaxMS {
			res.MaxMS, res.MaxAt = ms, t0.UnixMilli()
		}
		if d > 10*time.Millisecond {
			res.Over10++
		}
		if d > 100*time.Millisecond {
			res.Over100++
		}
	}
	if res.Writes > 0 {
		res.MeanMS = float64(total) / 1e6 / float64(res.Writes)
	}
	_ = json.NewEncoder(os.Stdout).Encode(res)
}
