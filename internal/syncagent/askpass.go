package syncagent

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/bfreis/caboose/internal/statesync"
)

// A sync runs with no terminal, so a prompt of git's (a username or a
// password for an HTTPS remote) or ssh's (a host key to accept, a
// passphrase) goes to the program GIT_ASKPASS and SSH_ASKPASS name, with
// SSH_ASKPASS_REQUIRE=force so that ssh uses it with no display. That
// program is this one, as `askpass PROMPT` (through a script, since
// neither variable takes arguments): it hands the prompt to the running
// sync over a Unix socket in a directory of its own, and the sync asks the
// host, whose caboose asks the person at its terminal. A sync nobody is
// watching refuses every prompt without asking, and the command fails as
// it would at a terminal nobody answered.

// AskpassEnv names the socket a sync's askpass connects to.
const AskpassEnv = "CABOOSE_ASKPASS"

// maxPrompt bounds a prompt and its answer, on the socket.
const maxPrompt = 64 << 10

// askpassMsg is a prompt (Prompt), or its answer (Answer and OK), on the
// socket.
type askpassMsg struct {
	Prompt string `json:"prompt,omitempty"`
	Answer string `json:"answer,omitempty"`
	OK     bool   `json:"ok,omitempty"`
}

// serveAskpass points s's git and ssh at Askpass, and answers each prompt
// with answer, until stop.
func serveAskpass(s *statesync.Syncer, p Paths, answer func(Ask) (string, bool)) (stop func(), err error) {
	dir, err := os.MkdirTemp("", "caboose-sync-")
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			os.RemoveAll(dir)
		}
	}()
	script := filepath.Join(dir, "askpass")
	text := "#!/bin/sh\nexec " + shellQuote(p.Exe) + " askpass \"$@\"\n"
	if err := os.WriteFile(script, []byte(text), 0o700); err != nil {
		return nil, err
	}
	sock := filepath.Join(dir, "sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return nil, err
	}
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		conns = map[net.Conn]bool{}
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns[c] = true
			mu.Unlock()
			// One at a time: git and ssh ask one question after another.
			answerOne(c, answer)
			mu.Lock()
			delete(conns, c)
			mu.Unlock()
		}
	}()
	s.Env = append(s.Env,
		"GIT_ASKPASS="+script, "SSH_ASKPASS="+script, "SSH_ASKPASS_REQUIRE=force",
		AskpassEnv+"="+sock)
	return func() {
		ln.Close()
		mu.Lock()
		for c := range conns {
			c.Close()
		}
		mu.Unlock()
		wg.Wait()
		os.RemoveAll(dir)
	}, nil
}

// askpassIO bounds how long a connection has to say its prompt, and to
// take its answer: anything in the sandbox can connect, and none may hold
// the sync up.
const askpassIO = 10 * time.Second

func answerOne(c net.Conn, answer func(Ask) (string, bool)) {
	defer c.Close()
	_ = c.SetReadDeadline(time.Now().Add(askpassIO))
	var m askpassMsg
	if err := readAskpass(c, &m); err != nil {
		return
	}
	a, ok := answer(Ask{Prompt: m.Prompt})
	data, _ := json.Marshal(askpassMsg{Answer: a, OK: ok})
	_ = c.SetWriteDeadline(time.Now().Add(askpassIO))
	_, _ = c.Write(append(data, '\n'))
}

func readAskpass(r io.Reader, m *askpassMsg) error {
	line, err := bufio.NewReader(io.LimitReader(r, maxPrompt)).ReadBytes('\n')
	if err != nil {
		return err
	}
	return json.Unmarshal(line, m)
}

// Askpass is caboose-agent askpass PROMPT: it asks the running sync, and
// prints the answer. It exits 1 when there is none: refused, or no sync
// to ask.
func Askpass(args []string, stdout, stderr io.Writer) int {
	sock := os.Getenv(AskpassEnv)
	if sock == "" {
		fmt.Fprintln(stderr, "caboose-agent: askpass: only a sync runs it")
		return 1
	}
	c, err := net.DialTimeout("unix", sock, 5*time.Second)
	if err != nil {
		fmt.Fprintf(stderr, "caboose-agent: askpass: %v\n", err)
		return 1
	}
	defer c.Close()
	prompt := strings.Join(args, " ")
	if len(prompt) > maxPrompt/2 {
		prompt = prompt[:maxPrompt/2]
	}
	data, _ := json.Marshal(askpassMsg{Prompt: prompt})
	if _, err := c.Write(append(data, '\n')); err != nil {
		fmt.Fprintf(stderr, "caboose-agent: askpass: %v\n", err)
		return 1
	}
	var m askpassMsg
	if err := readAskpass(c, &m); err != nil || !m.OK {
		return 1
	}
	if _, err := fmt.Fprintln(stdout, m.Answer); err != nil {
		return 1
	}
	return 0
}

// shellQuote is s as one sh word.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
