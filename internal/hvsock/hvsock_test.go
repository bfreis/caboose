package hvsock

import (
	"errors"
	"io"
	"net"
	"path/filepath"
	"testing"
	"time"
)

// serve is a vm.sock whose guest listens on port 7 only, and echoes.
func serve(t *testing.T) string {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "vm.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				port, err := Accept(c, time.Second)
				if err != nil || port != 7 {
					return
				}
				if Ready(c, 1<<30) != nil {
					return
				}
				_, _ = io.Copy(c, c)
			}()
		}
	}()
	return sock
}

// A dial reaches the port asked for, and what follows the handshake is the
// guest's, even when it came in the same write as the OK.
func TestDialEcho(t *testing.T) {
	sock := serve(t)
	c, err := Dial(sock, 7, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 4)
	if _, err := io.ReadFull(c, got); err != nil || string(got) != "ping" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestDialRefused(t *testing.T) {
	sock := serve(t)
	if _, err := Dial(sock, 8, time.Second); !errors.Is(err, ErrRefused) {
		t.Fatalf("err = %v, want ErrRefused", err)
	}
}

func TestDialNoSocket(t *testing.T) {
	if _, err := Dial(filepath.Join(t.TempDir(), "none"), 7, time.Second); err == nil {
		t.Fatal("dialled a socket that does not exist")
	}
}

// An OK and the guest's first bytes in one write: the reader must not take
// more than the line.
func TestReadLineLeavesTheRest(t *testing.T) {
	a, b := net.Pipe()
	go func() { _, _ = b.Write([]byte("OK 1\nrest")); b.Close() }()
	line, err := readLine(a)
	if err != nil || line != "OK 1" {
		t.Fatalf("line %q, %v", line, err)
	}
	rest, _ := io.ReadAll(a)
	if string(rest) != "rest" {
		t.Fatalf("rest %q", rest)
	}
}

func TestAcceptRefusesGarbage(t *testing.T) {
	for _, in := range []string{"HELLO 1\n", "CONNECT x\n", "CONNECT 99999999999\n", "CONNECT\n"} {
		a, b := net.Pipe()
		go func() { _, _ = b.Write([]byte(in)); b.Close() }()
		if _, err := Accept(a, time.Second); err == nil {
			t.Errorf("%q accepted", in)
		}
		a.Close()
	}
}
