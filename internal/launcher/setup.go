package launcher

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/datadir"
	"github.com/bfreis/caboose/internal/nofollow"
)

// SetupSections are the parts of caboose setup that can be run alone, in
// the order a whole run takes them.
var SetupSections = []string{"roots", "image", "isolation", "git", "sync"}

// Setup is `caboose setup [SECTION...]`: it sets up the environment,
// creating it first if it does not exist (after asking), then asks each
// section's questions, every one with what is there now as its default,
// and writes only what an answer changed. With no sections, all of them,
// and more: the container is brought up after the isolation (setupStart), and
// the run ends at the Claude login (setupLogin).
//
// It needs a terminal: there is no way to answer otherwise, and no answers
// are made up. Each section writes when its questions are done, so one cut
// short (^C, ^D) leaves the sections before it written and nothing of its
// own. Once a run completes, the environment's config.toml exists, which
// is what "set up" means to a launch and to doctor.
func (a *App) Setup(args []string) error {
	sections, err := setupSections(args)
	if err != nil {
		return err
	}
	c := a.Cfg
	if c.CabooseHome == "" {
		return Die("no CABOOSE_HOME, and no HOME to put it under: nowhere to keep an environment")
	}
	term, err := a.openTerminal()
	if err != nil {
		return Die("caboose setup asks questions, and there is no terminal to ask them on (%v);\n"+
			"       run it in one. Nothing was changed.", err)
	}
	defer term.Close()
	p := a.newSetupPrompter(term)
	a.inSetup = true

	err = a.setupRun(p, sections)
	if errors.Is(err, errNoAnswer) {
		fmt.Fprintln(a.Stderr)
		return Die("setup stopped: the steps finished before this one are written, nothing after")
	}
	return err
}

func (a *App) setupRun(p *prompter, sections []string) error {
	c := a.Cfg
	// A whole run also brings the container up and ends at the login.
	whole := slices.Equal(sections, SetupSections)
	p.steps = len(sections)
	if whole {
		p.steps += 2 // the container, and the login
	}
	p.banner("caboose setup", "environment "+c.Env)
	if p.term != nil {
		p.note("Each answer starts at what is there now. ^C stops; the steps finished before it stay written.")
	} else {
		p.note("Enter keeps the answer in [brackets], which is what is there now. ^C stops; the steps finished before it stay written.")
	}
	created, err := a.ensureEnvDir(p)
	if err != nil {
		return err
	}
	for _, s := range sections {
		switch s {
		case "roots":
			err = a.setupRoots(p)
		case "image":
			err = a.setupImage(p)
		case "isolation":
			if err = a.setupIsolation(p); err == nil && whole {
				err = a.setupStart(p)
			}
		case "git":
			err = a.setupGit(p)
		case "sync":
			err = a.setupSync(p)
		}
		if err != nil {
			return err
		}
	}
	p.blank()
	if err := a.writeConfigTemplate(p); err != nil {
		return err
	}
	p.ok("Environment '%s' is set up.", c.Env)
	rows := [][2]string{{"settings", a.short(filepath.Join(c.EnvDir, config.FileName))}}
	if c.Env != config.DefaultEnv {
		rows = append(rows, [2]string{"use it", fmt.Sprintf("caboose -e %s   (or CABOOSE_ENV=%s)", c.Env, c.Env)})
	}
	p.table(rows)
	if created && !whole {
		p.note("Its first launch builds its image, creates container %s, and asks for a Claude login.", c.Container)
	}
	if whole {
		return a.setupLogin(p)
	}
	return nil
}

// setupSections checks setup's arguments: section names, each once. The
// environment is caboose's own flag, before the command, and is refused
// here with the spelling that works rather than taught a second one.
func setupSections(args []string) ([]string, error) {
	if len(args) == 0 {
		return SetupSections, nil
	}
	seen := map[string]bool{}
	for i, arg := range args {
		switch {
		case arg == "-e" || arg == "--env" || strings.HasPrefix(arg, "--env="):
			name := strings.TrimPrefix(arg, "--env=")
			if name == arg {
				name = "NAME"
				if i+1 < len(args) {
					name = args[i+1]
				}
			}
			return nil, Die("the environment goes before the command: 'caboose -e %s setup'", name)
		case !isSetupSection(arg):
			return nil, Die("usage: caboose setup [%s]... ('%s' is not a section)", strings.Join(SetupSections, " | "), arg)
		case seen[arg]:
			return nil, Die("usage: caboose setup [%s]... ('%s' is named twice)", strings.Join(SetupSections, " | "), arg)
		}
		seen[arg] = true
	}
	// A whole run's order, whatever order they were named in.
	var out []string
	for _, s := range SetupSections {
		if seen[s] {
			out = append(out, s)
		}
	}
	return out, nil
}

func isSetupSection(name string) bool {
	for _, s := range SetupSections {
		if s == name {
			return true
		}
	}
	return false
}

// SetupCommand is the setup command for env, with section when it is not
// "": the -e form for any environment but the default one, which works
// whatever CABOOSE_ENV says.
func SetupCommand(env, section string) string {
	cmd := "caboose setup"
	if env != config.DefaultEnv {
		cmd = "caboose -e " + env + " setup"
	}
	if section != "" {
		cmd += " " + section
	}
	return cmd
}

// ensureEnvDir makes the environment's dir when it does not exist, once
// asked: the typo guard every other command applies (config.CheckEnv) is a
// question here. The default environment always exists; its dir is made
// when its config.toml is written.
func (a *App) ensureEnvDir(p *prompter) (created bool, err error) {
	c := a.Cfg
	if c.Env == config.DefaultEnv || (config.OSFS{}).IsDir(c.EnvDir) {
		return false, nil
	}
	p.blank()
	ok, err := p.yesNo(fmt.Sprintf("There is no environment '%s'. Create it?", c.Env), false)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, Die("nothing was changed")
	}
	if err := os.MkdirAll(filepath.Join(c.EnvDir, "data"), 0o700); err != nil {
		return false, Die("%v", err)
	}
	p.ok("Created environment '%s' in %s", c.Env, a.short(c.EnvDir))
	return true, nil
}

// writeConfig applies e to the environment's config.toml (the template,
// when there is none yet), line by line so that everything else in it
// stays. The result is parsed back first, and must say what e says, or
// nothing is written and the lines to add by hand are shown instead. The
// file is outside the data dir, and so never the container's to write: a
// plain replace by rename.
func (a *App) writeConfig(e config.Edit) (changed bool, err error) {
	c := a.Cfg
	path := filepath.Join(c.EnvDir, config.FileName)
	data, err := os.ReadFile(path)
	mode := os.FileMode(0o644)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		data = []byte(config.Template)
	case err != nil:
		return false, Die("reading %s: %v", path, err)
	default:
		if fi, err := os.Stat(path); err == nil {
			mode = fi.Mode().Perm()
		}
	}
	edited := config.EditFile(data, e)
	if string(edited) == string(data) {
		return false, nil
	}
	if err := config.CheckEdit(path, edited, e); err != nil {
		return false, Die("cannot edit %s safely (%v);\n"+
			"       nothing was written to it. Make this change by hand:\n%s", path, err, indent(e.Snippet(), "         "))
	}
	if err := os.MkdirAll(c.EnvDir, 0o700); err != nil {
		return false, Die("%v", err)
	}
	f, err := os.CreateTemp(c.EnvDir, "."+config.FileName+"-*")
	if err != nil {
		return false, Die("%v", err)
	}
	defer os.Remove(f.Name())
	_, err = f.Write(edited)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(f.Name(), mode)
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	if err != nil {
		return false, Die("writing %s: %v", path, err)
	}
	return true, nil
}

// writeConfigTemplate writes the environment's config.toml, every setting
// commented out at its default, unless there is one already: that one is
// the user's, and setup edits it only for what it asks.
func (a *App) writeConfigTemplate(p *prompter) error {
	c := a.Cfg
	path := filepath.Join(c.EnvDir, config.FileName)
	if _, err := os.Lstat(path); err == nil || !errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err := os.MkdirAll(c.EnvDir, 0o700); err != nil {
		return Die("%v", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return Die("%v", err)
	}
	_, err = f.WriteString(config.Template)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return Die("writing %s: %v", path, err)
	}
	p.ok("Wrote %s, with every setting commented out at its default", a.short(path))
	return nil
}

// setupGit asks for the sandbox's git identity and commit signing, and
// writes what changed into its git config. The defaults are the sandbox's
// own values; the host's are offered only where the sandbox has none.
func (a *App) setupGit(p *prompter) error {
	c := a.Cfg
	git := datadir.FindGit()
	if git == nil {
		return Die("setting the sandbox's git identity needs git on the host, to edit its config with")
	}
	// A launch's set-up, so the git config exists to be written.
	if err := a.prepareDataDir(); err != nil {
		return err
	}
	path := filepath.Join(c.DataDir, datadir.GitConfig)
	cur, err := datadir.ReadSandboxGit(c.DataDir, git)
	if errors.Is(err, nofollow.ErrNotPlain) {
		return Die("the sandbox's git config (%s) is a symlink or a hard link, or is reached through one,\n"+
			"       and setup never writes through those: remove it from inside (caboose shell, then\n"+
			"       rm ~/.config/git/config) and run '%s' again", path, SetupCommand(c.Env, "git"))
	}
	if err != nil {
		return Die("reading %s: %v", path, err)
	}
	host := datadir.HostIdentity(git)

	p.heading("Git", "Who commits made in the sandbox are by, and what signs them. Kept in "+a.short(path)+".")
	name, err := p.ask("Name (user.name)", or(cur.Name, host.Name))
	if err != nil {
		return err
	}
	email, err := p.ask("Email (user.email)", or(cur.Email, host.Email))
	if err != nil {
		return err
	}
	var changes []datadir.Change
	if name != "" && name != cur.Name {
		changes = append(changes, datadir.Change{Key: "user.name", Value: name})
	}
	if email != "" && email != cur.Email {
		changes = append(changes, datadir.Change{Key: "user.email", Value: email})
	}
	signing, err := a.setupSigning(p, git, cur.Signing)
	if err != nil {
		return err
	}
	changes = append(changes, signing...)

	if len(changes) == 0 {
		p.same("Nothing changed")
	} else {
		if err := datadir.WriteSandboxGit(c.DataDir, git, changes); err != nil {
			return Die("writing %s: %v", path, err)
		}
		var keys []string
		for _, ch := range changes {
			keys = append(keys, ch.Key)
		}
		p.ok("Wrote %s", strings.Join(dedup(keys), ", "))
	}
	if name == "" || email == "" {
		p.warn("The sandbox has no user.name or no user.email, so commits in it fail until both are set.")
	}
	return nil
}

// signChoice is one answer to setup's signing question.
type signChoice struct {
	label string
	// key is the literal public key to sign with; "" for keep, none and
	// paste.
	key               string
	keep, none, paste bool
	// fromHost is set on the host's own key, whose commit.gpgsign is the
	// default for signing every commit.
	fromHost bool
}

// setupSigning asks how the sandbox signs commits, and returns the changes
// that answer makes. The choices: keep what the sandbox has, the host's
// SSH signing key, a key the forwarded agent holds (listed only with the
// container running), a pasted key, or none.
func (a *App) setupSigning(p *prompter, git datadir.Git, cur datadir.SandboxSigning) ([]datadir.Change, error) {
	agentKeys, agentWhy := a.setupAgentKeys()
	hostKey, skipped := datadir.HostSigningKey(git)
	curPub := ""
	if strings.EqualFold(cur.Format, "ssh") {
		curPub = cur.PublicKey()
	}

	var choices []signChoice
	def := -1
	if cur.Key != "" {
		label := "Keep the current key: " + keyName(curPub)
		if curPub == "" {
			label = fmt.Sprintf("Keep the current setting (gpg.format %s, user.signingkey %s)", or(cur.Format, "openpgp"), cur.Key)
		}
		def = len(choices)
		choices = append(choices, signChoice{label: label, keep: true})
	}
	offered := []string{curPub}
	isOffered := func(k string) bool { return holds(offered, k) }
	if hostKey != "" && !isOffered(hostKey) {
		if def < 0 {
			def = len(choices)
		}
		choices = append(choices, signChoice{label: "The host's signing key: " + keyName(hostKey), key: hostKey, fromHost: true})
		offered = append(offered, hostKey)
	}
	for _, k := range agentKeys {
		if !isOffered(k) {
			choices = append(choices, signChoice{label: "From the forwarded agent: " + keyName(k), key: k})
			offered = append(offered, k)
		}
	}
	choices = append(choices, signChoice{label: "Paste a public key, or the path of a .pub file on this host", paste: true})
	if def < 0 {
		def = len(choices)
	}
	choices = append(choices, signChoice{label: "None: commits are not signed", none: true})

	if skipped != "" {
		p.warn("The host signs commits with SSH, but its key cannot be used here: %s", skipped)
	}
	if agentWhy != "" {
		p.note("The forwarded agent's keys are not listed: %s.", agentWhy)
	}
	labels := make([]string, len(choices))
	for i, ch := range choices {
		labels[i] = ch.label
	}
	var ch signChoice
	for {
		i, err := p.choose("Sign commits made in the sandbox with", labels, def)
		if err != nil {
			return nil, err
		}
		ch = choices[i]
		if !ch.paste {
			break
		}
		key, err := a.askKey(p)
		if err != nil {
			return nil, err
		}
		if key != "" {
			ch.key = key
			break
		}
	}

	switch {
	case ch.none:
		if cur.Key == "" && cur.CommitSign == "" {
			return nil, nil
		}
		return datadir.NoSigningChanges(), nil
	case ch.keep || curPub != "" && holds([]string{curPub}, ch.key):
		every, err := p.yesNo("Sign every commit? (commit.gpgsign)", cur.Signs())
		if err != nil || every == cur.Signs() {
			return nil, err
		}
		return []datadir.Change{{Key: "commit.gpgsign", Value: fmt.Sprint(every)}}, nil
	}
	if agentWhy == "" && !holds(agentKeys, ch.key) {
		p.warn("The forwarded agent does not hold %s, so signing fails until it does: add it to the host's agent.", keyName(ch.key))
	}
	everyDef := true
	switch {
	case cur.CommitSign != "":
		everyDef = cur.Signs()
	case ch.fromHost:
		everyDef = datadir.SandboxSigning{CommitSign: git.GetGlobal("commit.gpgsign")}.Signs()
	}
	every, err := p.yesNo("Sign every commit? (commit.gpgsign)", everyDef)
	if err != nil {
		return nil, err
	}
	changes := datadir.SigningChanges(ch.key)
	if cur.CommitSign != "" || every {
		changes = append(changes, datadir.Change{Key: "commit.gpgsign", Value: fmt.Sprint(every)})
	}
	return changes, nil
}

// askKey reads a pasted public key, or the path of a .pub file; "" (an
// empty answer) goes back to the list.
func (a *App) askKey(p *prompter) (string, error) {
	for {
		s, err := p.ask("Public key, or the path of a .pub file (empty to go back)", "")
		if err != nil || s == "" {
			return "", err
		}
		key, why := datadir.LiteralSSHKey(s)
		if key != "" {
			return key, nil
		}
		// LiteralSSHKey reads anything that is not a key as a path.
		if !strings.Contains(s, "/") && !strings.HasPrefix(s, "~") {
			why = "not an SSH public key (ssh-ed25519 AAAA..., as in a .pub file), nor a path"
		}
		p.fail("%s", strings.TrimPrefix(why, "user.signingkey "))
	}
}

// setupAgentKeys lists the public keys the container's forwarded agent
// holds, or says why they are not listed.
func (a *App) setupAgentKeys() (keys []string, why string) {
	if a.state() != "running" {
		return nil, "the container is not running. Start it (run caboose in a project), then run '" +
			SetupCommand(a.Cfg.Env, "git") + "' again to choose one of them"
	}
	out, err := a.Docker.Output("exec", a.Cfg.Container, "ssh-add", "-L")
	if err != nil {
		if strings.Contains(out, "no identities") {
			return nil, "the forwarded agent holds no keys"
		}
		return nil, "'ssh-add -L' in the container failed ('caboose doctor' says more)"
	}
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); datadir.IsPublicKey(l) {
			keys = append(keys, l)
		}
	}
	if len(keys) == 0 {
		return nil, "the forwarded agent holds no keys"
	}
	return keys, ""
}

// dedup drops repeats from s, keeping the first of each.
func dedup(s []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, v := range s {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}
