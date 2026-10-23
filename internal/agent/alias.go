package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/bfreis/caboose/internal/agentproto"
)

// The exit statuses of `caboose-agent run` for an alias that did not run:
// the request waits for a person on the host (EX_TEMPFAIL), the host
// refused it (EX_NOPERM), there is no such alias, or it could not start.
const (
	ExitPending     = 75
	ExitDenied      = 77
	ExitUnknown     = agentproto.ExitNotFound
	ExitUnavailable = agentproto.ExitCannotRun
)

// AliasRun runs the host's alias argv names ([NAME, KEY=VALUE...]),
// through the link on socket, with stdio, and returns its exit status. A
// run the host refused is the status for why (ExitPending, ExitDenied,
// ExitUnknown, ExitUnavailable) and a *Refused saying it. dir, the
// directory it was asked from, is only for the host's log. Done, ctx
// closes the connection, which ends the alias on the host.
func AliasRun(ctx context.Context, socket string, argv []string, dir string, stdio agentproto.ExecIO) (int, error) {
	if _, _, err := agentproto.ParseAliasArgv(argv); err != nil {
		return ExitDenied, err
	}
	req := agentproto.ExecRequest{Argv: argv, Dir: dir, Stdin: true}
	code, err := execOnHost(ctx, socket, agentproto.Message{Op: agentproto.OpAliasRun, Exec: &agentproto.ExecRequest{Argv: argv, Dir: dir}}, req, stdio)
	var r *Refused
	if errors.As(err, &r) {
		return aliasExit(r.Reason), err
	}
	return code, err
}

// aliasExit is the exit status for a refusal's reason.
func aliasExit(reason string) int {
	switch reason {
	case agentproto.AliasPending:
		return ExitPending
	case agentproto.AliasDenied:
		return ExitDenied
	case agentproto.AliasUnknown:
		return ExitUnknown
	}
	return ExitUnavailable
}

// Aliases are the host's aliases, through the link on socket.
func Aliases(socket string) ([]agentproto.AliasInfo, error) {
	r, err := ask(socket, agentproto.Message{Op: agentproto.OpAliases})
	if err != nil {
		return nil, err
	}
	return r.Aliases, nil
}

// runCommand is `caboose-agent run NAME [KEY=VALUE...]`: the alias's exit
// status, or the status for why it did not run; 130 or 143 when this was
// interrupted or terminated.
func runCommand(args []string, stdin io.Reader, stdout, stderr io.Writer, usage func() int) int {
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return usage()
	}
	if _, _, err := agentproto.ParseAliasArgv(args); err != nil {
		fmt.Fprintf(stderr, "caboose-agent: run: %v\n", err)
		return 2
	}
	dir, _ := os.Getwd()
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type result struct {
		code int
		err  error
	}
	done := make(chan result, 1)
	go func() {
		code, err := AliasRun(ctx, linkSocket, args, dir, agentproto.ExecIO{Stdin: stdin, Stdout: stdout, Stderr: stderr})
		done <- result{code, err}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			fmt.Fprintf(stderr, "caboose-agent: run: %s\n", printable(r.err.Error()))
		}
		return r.code
	case sig := <-sigs:
		// Closing the connection ends the alias on the host.
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
		}
		if sig == syscall.SIGTERM {
			return 128 + int(syscall.SIGTERM)
		}
		return 128 + int(syscall.SIGINT)
	}
}

// aliasesCommand is `caboose-agent aliases [--json]`.
func aliasesCommand(args []string, stdout, stderr io.Writer, usage func() int) int {
	asJSON := false
	switch {
	case len(args) == 1 && args[0] == "--json":
		asJSON = true
	case len(args) != 0:
		return usage()
	}
	list, err := Aliases(linkSocket)
	if err != nil {
		fmt.Fprintf(stderr, "caboose-agent: aliases: %s\n", printable(err.Error()))
		return 1
	}
	if asJSON {
		if list == nil {
			list = []agentproto.AliasInfo{}
		}
		b, _ := json.MarshalIndent(list, "", "  ")
		fmt.Fprintf(stdout, "%s\n", b)
		return 0
	}
	writeAliases(stdout, list)
	return 0
}

// writeAliases lists aliases for a person: each name and description,
// then its parameters and whether it asks first.
func writeAliases(w io.Writer, list []agentproto.AliasInfo) {
	if len(list) == 0 {
		fmt.Fprintln(w, "the host defines no aliases ([alias.NAME] in its config.toml)")
		return
	}
	for _, a := range list {
		fmt.Fprintf(w, "%s", printable(a.Name))
		if a.Description != "" {
			fmt.Fprintf(w, "  %s", printable(a.Description))
		}
		fmt.Fprintln(w)
		for _, k := range slices.Sorted(maps.Keys(a.Params)) {
			fmt.Fprintf(w, "    %s=%s\n", printable(k), printable(strings.Join(a.Params[k], "|")))
		}
		if a.Asks {
			fmt.Fprintln(w, "    asks a person on the host before it runs")
		}
	}
}
