package main

import (
	"fmt"
	"strings"
)

// argKind is how a command takes what follows it on the command line.
type argKind int

const (
	// noArgs: nothing; a stray word is more likely a mistyped command than
	// something to ignore.
	noArgs argKind = iota
	// ownArgs: the command's own, which it checks itself. -h or --help
	// anywhere among them is caboose's help for it.
	ownArgs
	// passArgs: handed on to another program (bash, docker). Only a -h or
	// --help in first place is caboose's help for the command.
	passArgs
	// claudeArgs: claude's, every one of them, --help included.
	claudeArgs
)

// Command is one of the launcher's subcommands: what `caboose help` lists,
// and `caboose help NAME` (or `caboose NAME --help`) explains.
type Command struct {
	Name string
	// Usage is what follows the name in a synopsis.
	Usage string
	// Summary is the command's line in `caboose help`.
	Summary string
	// Help is the rest of `caboose help NAME`, wrapped as it is printed.
	Help string
	Args argKind
}

// Commands are the launcher's subcommands, in the order `caboose help`
// lists them. With none, caboose starts or attaches the project's session.
var Commands = []Command{
	{Name: "claude", Usage: "[ARGS...]", Args: claudeArgs,
		Summary: "start or attach the session, passing ARGS to claude",
		Help: `Like plain 'caboose', but ARGS go to claude untouched: 'caboose claude
-p "..."' runs a one-shot prompt in the sandbox, 'caboose claude --resume'
picks a conversation to resume, and 'caboose claude --help' is claude's own
help. Everything after 'claude' is claude's, including anything spelled
like one of caboose's flags; those go before 'claude'.

ARGS only apply to a session that starts now: attaching to one that is
already running ignores them, and says so.`},
	{Name: "setup", Usage: "[SECTION]...", Args: ownArgs,
		Summary: "set up the environment (creating it, after asking)",
		Help: `Asks for what the defaults do not know, on the terminal, each question
showing what is there now as its default: the roots, what the sandbox
is built on, what isolates it (and, on OrbStack, getting gVisor's runsc),
its git identity and commit signing, and whether and where to sync. Naming
SECTIONs -- roots, image, isolation, git, sync -- asks only those. A
whole run also brings the sandbox up and ends at the Claude login. Only
what an answer changed is written, so a re-run is safe; with no terminal
it refuses.`},
	{Name: "apply", Args: noArgs,
		Summary: "review the changes sessions proposed, and apply them",
		Help: `A session in the sandbox cannot change what the sandbox is: it proposes
the change instead, into ~/.caboose-proposals -- packages to add or remove
under an apko image, a Dockerfile section under a dockerfile one, or
another root. (What it keeps of its home is its own sandbox config's,
which it edits itself.) This shows each proposal whole, and applies it,
leaves it pending or deletes it as you say. Nothing else in config.toml
can be proposed, and a root that would hand the sandbox your home, a
hidden directory of it or caboose's own state is refused. Packages are
built before anything is written, so a list that does not build stays
pending; after a Dockerfile change it offers to build. Then it offers the
restart that moves the sandbox onto the changes (which ends running
sessions). With no terminal it refuses.`},
	{Name: "doctor", Usage: "[--offline]", Args: ownArgs,
		Summary: "what is wrong, and the command that fixes each problem",
		Help: `Checks the whole environment -- configuration, data dir, Docker engine,
image, sandbox, Claude Code, sessions, SSH agent, git identity and
signing, sync -- and lists each problem with the command that fixes it.
It changes nothing. --offline skips fetching from the sync remote.
Exits 0 with no problems, 1 with any, 2 when it could not run.`},
	{Name: "status", Args: noArgs,
		Summary: "the sandbox, SSH agent, live sessions and disk use"},
	{Name: "version", Args: noArgs,
		Summary: "the launcher's version, and if the image matches it",
		Help: `The launcher's version, commit and build date, how it was installed, and
whether the image exists and was built from this launcher's files, on the
base in use. Also: caboose --version.`},
	{Name: "update", Args: ownArgs,
		Summary: "update caboose to the latest release now",
		Help: `Downloads the latest release, checks it against its checksums, and
installs it next to this one. An install made by install.sh also does this
by itself, at most daily, in the background; CABOOSE_NO_AUTO_UPDATE=1 turns
that off. A build from a checkout never updates itself.`},
	{Name: "build", Usage: "[ARGS]", Args: passArgs,
		Summary: "build the image: base, checked, then caboose's layer",
		Help: `Builds the base the image profile names (packages with apko, a
Dockerfile's dir, or pulls a ref), checks it, and builds the layer on it.
ARGS go to the docker builds (--no-cache, --progress=plain, -q), except
--pull, which resolves an apko profile's packages again, pulls a ref, or
goes to a Dockerfile's build. The sandbox is not touched: 'caboose
restart' moves it onto the new image.`},
	{Name: "check-image", Usage: "[IMAGE]", Args: ownArgs,
		Summary: "whether IMAGE can be the sandbox's base",
		Help: `Runs a throwaway container of IMAGE (default: the base in use) and lists
each requirement as met or not. Exits 0 when all are, 1 when one is not,
2 when it could not check. Under isolation vm the container runs in the
builder VM caboose build uses, which pulls a named image from its registry:
it cannot see a docker engine's images.`},
	{Name: "restart", Args: noArgs,
		Summary: "recreate the sandbox (asks before ending sessions)",
		Help: `Recreates the sandbox, building the image first when it is stale, so
it picks up a rebuilt image, changed roots, or a change to what the sandbox
config keeps. Every running
session ends: it lists them and asks first. CABOOSE_FORCE=1 skips the question;
with no terminal it refuses.`},
	{Name: "stop", Args: noArgs,
		Summary: "stop the sandbox (asks before ending sessions)"},
	{Name: "detach", Args: noArgs,
		Summary: "detach every terminal from this project's sessions"},
	{Name: "logs", Usage: "[ARGS]", Args: passArgs,
		Summary: "the sandbox's log (ARGS go to docker logs, or --tail N)"},
	{Name: "shell", Usage: "[ARGS]", Args: passArgs,
		Summary: "a bash prompt in the sandbox (ARGS go to bash)",
		Help: `bash in the sandbox, at this directory's path there; ARGS go to it, so
'caboose shell -c CMD' runs CMD. It has a terminal when caboose runs on
one; from a script or a pipe it has none, CMD's output comes out as it
is, and a bash given no -c reads its commands from stdin
('caboose shell < script').`},
	{Name: "link", Usage: "[--restart]", Args: ownArgs,
		Summary: "forward the sandbox's ports, open its URLs",
		Help: `The host's end of the link to caboose-agent in the sandbox: while it
runs, a port something listens on in it is forwarded to
the same port on this machine's localhost when forward_ports allows it,
and 'caboose-agent open URL' or 'notify TEXT' in the sandbox opens an
http(s) URL here (after asking, as open_urls says) or shows a notification.
Every launch starts one in the background, logging to the data dir's
link.log, and replaces one running with other settings; run in a
terminal, it logs there instead. One runs at a time. It rereads
config.toml by itself when that changes. --restart stops the running one
and starts another in the background.`},
	{Name: "prune", Usage: "[--docker]", Args: ownArgs,
		Summary: "delete old Claude Code versions and unused packages",
		Help: `Deletes the installed Claude Code versions beyond the newest
keep_versions in [session], and says what the rest take up. First, on
this machine, it removes from the package cache apko builds share
(CABOOSE_HOME/cache/apk) every package no environment's lock names, and
every index but the newest, keeping anything fetched in the last hour,
and says what that freed. A lock it cannot read could name anything, so
then it leaves the cache alone, and says which lock.

--docker, under isolation vm, deletes the disk the sandbox's own dockerd
keeps everything on -- images, containers, volumes, build cache -- for an
empty one, which caboose restart keeps otherwise. It says the size it
frees and asks first (no by default); a running VM is stopped first,
ending its sessions, which it lists. CABOOSE_FORCE=1 skips the question; with no
terminal it refuses. Under docker and gvisor the sandbox has no dockerd of
its own, so there is nothing of caboose's to delete.`},
	{Name: "sync", Usage: "[ARGS]", Args: ownArgs,
		Summary: "sync what the sandbox config names with other machines",
		Help: `caboose sync [--remote URL] | status | add PATH | rm PATH

Sends this machine's changes since the last sync to the remote, takes the
other machines', and writes back only the files that changed. What syncs
is the sandbox config's rules (~/.config/caboose/sandbox.toml, which syncs
too). --remote URL sets the remote first (use a private repo). It refuses
while a session is running (CABOOSE_FORCE=1 overrides).

'status' shows what a sync would send and take, changing nothing. 'add
PATH' makes a path of the sandbox's home (~/...) sync, keeping it too when
nothing keeps it yet; 'rm PATH' stops a rule, leaving the files.`},
	{Name: "env", Usage: "[list]", Args: ownArgs,
		Summary: "list environments ('caboose -e NAME setup' makes one)"},
	{Name: "help", Usage: "[COMMAND]", Args: ownArgs,
		Summary: "this, or what COMMAND does"},
}

// lookup finds a command by name.
func lookup(name string) (Command, bool) {
	for _, c := range Commands {
		if c.Name == name {
			return c, true
		}
	}
	return Command{}, false
}

// Invocation is a parsed command line.
type Invocation struct {
	// Env is --env / -e, or "" when none was given.
	Env string
	// Session is --session, or "" when none was given.
	Session string
	// Command is one of Commands, or "" for the default: start or attach
	// the project's session, with no arguments for claude.
	Command string
	// Args are what follows the command.
	Args []string
}

// UsageError is a command line caboose does not accept: Msg says why, and
// Hint, when set, what to type instead.
type UsageError struct{ Msg, Hint string }

func (e *UsageError) Error() string { return e.Msg }

// Parse hand-parses argv (without the program name). No flag library: what
// follows `claude` must reach claude untouched, and what follows shell,
// logs or build, bash or docker.
//
// First come caboose's own flags, in any order: --env NAME (-e NAME,
// --env=NAME), --session NAME (--session=NAME), --help (-h) and --version.
// Then, optionally, a command and its arguments. Anything else is refused,
// with what to type instead: nothing reaches claude but through `caboose
// claude`.
func Parse(argv []string) (Invocation, error) {
	var inv Invocation
	var err error
	all := argv
flags:
	for len(argv) > 0 {
		switch flag := argv[0]; {
		case flag == "-e" || flag == "--env" || strings.HasPrefix(flag, "--env="):
			inv.Env, argv, err = flagValue(argv, "--env")
		case flag == "--session" || strings.HasPrefix(flag, "--session="):
			inv.Session, argv, err = flagValue(argv, "--session")
		case flag == "-h" || flag == "--help":
			// `caboose --help status` asks about status, as `caboose help
			// status` does.
			inv.Command, inv.Args = "help", argv[1:]
			return inv, checkHelp(inv.Args)
		case flag == "--version":
			if len(argv) > 1 {
				return inv, &UsageError{Msg: fmt.Sprintf("--version takes nothing after it (got %s)", quoteArgs(argv[1:]))}
			}
			inv.Command = "version"
			return inv, nil
		default:
			break flags
		}
		if err != nil {
			return inv, err
		}
	}
	if len(argv) == 0 {
		return inv, nil
	}
	name, rest := argv[0], argv[1:]
	cmd, ok := lookup(name)
	if !ok {
		return inv, unknown(name, all[len(all)-len(argv):])
	}
	inv.Command, inv.Args = name, rest
	switch cmd.Args {
	case claudeArgs:
		return inv, nil
	case passArgs:
		if len(rest) > 0 && isHelpFlag(rest[0]) {
			inv.Command, inv.Args = "help", []string{name}
		}
		return inv, nil
	}
	for _, a := range rest {
		if isHelpFlag(a) {
			inv.Command, inv.Args = "help", []string{name}
			return inv, nil
		}
	}
	switch {
	case cmd.Args == noArgs && len(rest) > 0:
		return inv, &UsageError{
			Msg:  fmt.Sprintf("%s takes no arguments (got %s)", name, quoteArgs(rest)),
			Hint: "see 'caboose help " + name + "'",
		}
	case name == "help":
		return inv, checkHelp(rest)
	}
	return inv, nil
}

func isHelpFlag(a string) bool { return a == "-h" || a == "--help" }

// checkHelp refuses `caboose help` with more than one word, or a word that
// is no command.
func checkHelp(args []string) error {
	switch {
	case len(args) > 1:
		return &UsageError{Msg: fmt.Sprintf("help takes at most one command (got %s)", quoteArgs(args))}
	case len(args) == 1:
		if _, ok := lookup(args[0]); !ok {
			return unknown(args[0], nil)
		}
	}
	return nil
}

// unknown is the error for a word caboose does not know where a command
// goes. from, when set, is the command line from that word on, which is
// offered to claude instead: it is most often something meant for it.
func unknown(word string, from []string) error {
	var msg string
	if strings.HasPrefix(word, "-") {
		msg = fmt.Sprintf("unknown flag %s", word)
	} else {
		msg = fmt.Sprintf("unknown command '%s'", word)
		if near := nearest(word); near != "" {
			msg += fmt.Sprintf(" -- did you mean '%s'?", near)
		}
	}
	hint := "see 'caboose --help'"
	if from != nil {
		hint = "for claude: caboose claude " + quoteArgs(from) + "\n" + hint
	}
	return &UsageError{Msg: msg, Hint: hint}
}

// nearest is the command word most likely meant -- one or two edits away,
// or the word a prefix of it -- or "".
func nearest(word string) string {
	best, bestD := "", 3
	for _, c := range Commands {
		d := distance(word, c.Name)
		if len(word) >= 3 && strings.HasPrefix(c.Name, word) {
			d = 1
		}
		if d < bestD {
			best, bestD = c.Name, d
		}
	}
	return best
}

// distance is the Levenshtein distance between a and b.
func distance(a, b string) int {
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(b)]
}

// quoteArgs spells args as a shell would need them typed.
func quoteArgs(args []string) string {
	q := make([]string, len(args))
	for i, a := range args {
		q[i] = shellQuote(a)
	}
	return strings.Join(q, " ")
}

func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("_-./:=@%+,", r)) {
			return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
		}
	}
	return s
}

// flagValue takes a flag's value from `--flag VALUE` or `--flag=VALUE`,
// returning what is left of argv.
func flagValue(argv []string, flag string) (string, []string, error) {
	if v, ok := strings.CutPrefix(argv[0], flag+"="); ok && argv[0] != flag {
		if v == "" {
			return "", nil, &UsageError{Msg: flag + " needs a name"}
		}
		return v, argv[1:], nil
	}
	if len(argv) < 2 || argv[1] == "" {
		return "", nil, &UsageError{Msg: flag + " needs a name"}
	}
	return argv[1], argv[2:], nil
}

// Usage is `caboose help`.
func Usage() string {
	var b strings.Builder
	b.WriteString(`caboose: Claude Code in a long-lived Docker sandbox, with its own account,
settings and plugins.

Usage:
  caboose [FLAGS]                    start or attach this project's session
  caboose [FLAGS] claude [ARGS...]   the same, passing ARGS to claude
  caboose [FLAGS] COMMAND [ARGS]     one of the commands below

Run it from a project under a root (see 'caboose help setup').

Commands:
`)
	width := 0
	for _, c := range Commands {
		width = max(width, len(synopsis(c)))
	}
	for _, c := range Commands {
		fmt.Fprintf(&b, "  %-*s  %s\n", width, synopsis(c), c.Summary)
	}
	b.WriteString(`
Flags, before the command:
  -e, --env NAME       the environment (default: CABOOSE_ENV, else "default")
      --session NAME   name the tmux session outright (default: CABOOSE_SESSION)
  -h, --help           this help; 'caboose help COMMAND' for one command
      --version        the launcher's version, as 'caboose version'

Anything for claude goes after 'claude': caboose claude -p "...".
When something is wrong: caboose doctor.
`)
	return b.String()
}

// CommandUsage is `caboose help NAME`, for one of Commands.
func CommandUsage(name string) string {
	c, _ := lookup(name)
	var b strings.Builder
	fmt.Fprintf(&b, "Usage: caboose [FLAGS] %s\n\n%s.\n", synopsis(c), upperFirst(c.Summary))
	if c.Help != "" {
		b.WriteString("\n" + c.Help + "\n")
	}
	b.WriteString("\nFlags, before the command, are caboose's own: see 'caboose --help'.\n")
	return b.String()
}

// synopsis is a command's name and what it takes.
func synopsis(c Command) string { return strings.TrimSpace(c.Name + " " + c.Usage) }

func upperFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
