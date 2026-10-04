package agentproto

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// The exec port runs one command per connection, in a vm guest, where
// there is no docker exec. The host dials, starts a Session as the opener,
// sends TypeHello and a TypeExec, and opens the command's streams: one
// named StreamTTY with a terminal, else StreamStdout, StreamStderr and,
// when the request has Stdin, StreamStdin. The agent sends one TypeExit
// once the command's output is all sent, and the session ends.
const (
	StreamTTY    = "tty"
	StreamStdin  = "stdin"
	StreamStdout = "stdout"
	StreamStderr = "stderr"
)

// ExecRequest is one command, what docker exec's flags say today.
type ExecRequest struct {
	Argv []string `json:"argv"`
	// Env is NAME=VALUE, over the sandbox's own.
	Env []string `json:"env,omitempty"`
	// Dir is where it runs: "" for the sandbox user's home.
	Dir string `json:"dir,omitempty"`
	// User is "UID" or "UID:GID": "" for the sandbox's own user.
	User  string `json:"user,omitempty"`
	Stdin bool   `json:"stdin,omitempty"`
	TTY   bool   `json:"tty,omitempty"`
	// Rows and Cols size the terminal at the start, with TTY.
	Rows uint16 `json:"rows,omitempty"`
	Cols uint16 `json:"cols,omitempty"`
}

// ExitCannotRun and ExitNotFound are the statuses of a command that did
// not start, as docker exec (and a shell) gives them.
const (
	ExitCannotRun = 126
	ExitNotFound  = 127
)

// ExecIO is the host's side of a command's streams.
type ExecIO struct {
	// Stdin is read only when the request has Stdin.
	Stdin io.Reader
	// Stdout gets the command's output, and with a TTY everything the
	// terminal shows; Stderr gets its errors, and what the agent says
	// when the command cannot start.
	Stdout, Stderr io.Writer
	// Resize carries the terminal's new size, rows then columns.
	Resize <-chan [2]uint16
}

// Exec runs req at the exec port on the other end of conn, and returns its
// exit status (0 to 255). It returns once the command's output is all
// written, and closes conn.
func Exec(conn io.ReadWriteCloser, req ExecRequest, stdio ExecIO) (int, error) {
	if len(req.Argv) == 0 {
		return 0, errors.New("agentproto: exec of nothing")
	}
	s := NewSession(conn, conn, true)
	defer s.Close()
	if err := s.Send(Message{Type: TypeHello, Version: Version}); err != nil {
		return 0, err
	}
	r := req
	if err := s.Send(Message{Type: TypeExec, Exec: &r}); err != nil {
		return 0, err
	}

	var out sync.WaitGroup
	open := func(name string) (*Stream, error) {
		h, _ := json.Marshal(StreamHeader{Name: name})
		return s.Open(h)
	}
	copyOut := func(st *Stream, w io.Writer) {
		out.Add(1)
		go func() {
			defer out.Done()
			if w == nil {
				w = io.Discard
			}
			_, _ = io.Copy(w, st)
		}()
	}
	copyIn := func(st *Stream) {
		if !req.Stdin || stdio.Stdin == nil {
			_ = st.CloseWrite()
			return
		}
		go func() {
			// Not waited for: a terminal's stdin reads on after the
			// command is gone.
			_, _ = io.Copy(st, stdio.Stdin)
			_ = st.CloseWrite()
		}()
	}
	if req.TTY {
		st, err := open(StreamTTY)
		if err != nil {
			return 0, err
		}
		copyIn(st)
		copyOut(st, stdio.Stdout)
	} else {
		if req.Stdin {
			st, err := open(StreamStdin)
			if err != nil {
				return 0, err
			}
			copyIn(st)
		}
		for _, o := range []struct {
			name string
			w    io.Writer
		}{{StreamStdout, stdio.Stdout}, {StreamStderr, stdio.Stderr}} {
			st, err := open(o.name)
			if err != nil {
				return 0, err
			}
			_ = st.CloseWrite()
			copyOut(st, o.w)
		}
	}

	if stdio.Resize != nil {
		go func() {
			for {
				select {
				case sz := <-stdio.Resize:
					_ = s.Send(Message{Type: TypeResize, Rows: sz[0], Cols: sz[1]})
				case <-s.Done():
					return
				}
			}
		}()
	}

	for b := range s.Control() {
		m, err := Decode(b)
		if err != nil {
			return 0, err
		}
		switch m.Type {
		case TypeHello:
			if m.Version != Version {
				return 0, fmt.Errorf("agentproto: the agent speaks version %d, not %d", m.Version, Version)
			}
		case TypeExit:
			// The output's frames came before this one: what is left
			// is only copying them out.
			out.Wait()
			if m.Error != "" && stdio.Stderr != nil {
				fmt.Fprintf(stdio.Stderr, "caboose-agent: %.500s\n", m.Error)
			}
			if m.Code < 0 || m.Code > 255 {
				return 0, fmt.Errorf("agentproto: exit status %d", m.Code)
			}
			return m.Code, nil
		}
	}
	return 0, fmt.Errorf("agentproto: the command's connection ended with no exit status (%v)", s.Err())
}
