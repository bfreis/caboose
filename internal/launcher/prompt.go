package launcher

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"

	"github.com/bfreis/caboose/internal/tty"
)

// errNoAnswer is a question left without an answer: the terminal closed,
// or ^D (^C, in a form).
var errNoAnswer = errors.New("no answer")

// prompter asks setup's questions, and says everything setup says around
// them (setup_ui.go). On a terminal the questions are huh forms, read from
// term; anywhere else -- a test, TERM=dumb, ACCESSIBLE -- they are plain
// lines, read from in, one answer a line.
type prompter struct {
	*ui
	in *bufio.Reader
	// raw is where ui writes, as given, for forms: bubbletea needs the
	// terminal itself.
	raw io.Writer
	// term is the terminal forms read from; nil asks in plain lines.
	term io.Reader
}

// newPrompter asks in plain lines, read from in; out takes colors only
// when it is a terminal (and NO_COLOR is not set).
func newPrompter(in io.Reader, out io.Writer) *prompter {
	return &prompter{ui: newUI(out, defaultWidth), in: bufio.NewReader(in), raw: out}
}

// newSetupPrompter is setup's prompter: forms when both the answers and
// what is said are on a terminal, wrapped to its width.
func (a *App) newSetupPrompter(term io.Reader) *prompter {
	p := newPrompter(term, a.Stderr)
	tf, ok := term.(*os.File)
	w := termWidth(a.Stderr, defaultWidth)
	if !ok || !tty.IsTerminal(tf.Fd()) || w == 0 {
		return p
	}
	p.width = w
	if a.getenv("TERM") != "dumb" && a.getenv("ACCESSIBLE") == "" {
		p.term = term
	}
	return p
}

// form runs one field as a form, and says what was answered in one line.
func (p *prompter) form(field huh.Field, question func() string, answer func() string) error {
	f := huh.NewForm(huh.NewGroup(field)).WithWidth(p.width - 2).WithShowHelp(true).WithTheme(formTheme)
	f.SubmitCmd = quitAfterFrame(tea.Quit)
	f.CancelCmd = quitAfterFrame(tea.Interrupt)
	m := &formModel{f: f}
	_, err := tea.NewProgram(m, tea.WithInput(p.term), tea.WithOutput(p.raw)).Run()
	// The last frame drawn is m.height blank lines, the cursor on the last
	// of them: back to the first, where the form started.
	if m.height > 1 {
		fmt.Fprintf(p.raw, "\x1b[%dA", m.height-1)
	}
	fmt.Fprint(p.raw, "\r\x1b[J")
	if f.State == huh.StateAborted || errors.Is(err, tea.ErrInterrupted) {
		return errNoAnswer
	}
	if err != nil {
		return err
	}
	p.answered(question(), answer())
	return nil
}

// formTheme is huh's own, indented as setup's prose is.
var formTheme = huh.ThemeFunc(func(isDark bool) *huh.Styles {
	s := huh.ThemeCharm(isDark)
	s.Form.Base = s.Form.Base.PaddingLeft(2)
	return s
})

// quitAfterFrame is quit, once the renderer has had time to draw the frame
// a form ends on (formModel.View).
func quitAfterFrame(quit func() tea.Msg) tea.Cmd {
	return func() tea.Msg {
		time.Sleep(50 * time.Millisecond)
		return quit()
	}
}

// formModel runs a huh form as huh's own Run does, but ends it on blank
// lines. Bubble Tea keeps the last frame it drew, and a frame that shrinks
// leaves all but the last line of the one before: a form done draws
// nothing, and would stay on the screen whole. So once done it draws as
// many blank lines as it last took, which overwrites it, and quits a frame
// later; form takes the cursor back up over them.
type formModel struct {
	f *huh.Form
	// height is the lines the form last took.
	height int
}

func (m *formModel) Init() tea.Cmd { return m.f.Init() }

func (m *formModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	next, cmd := m.f.Update(msg)
	if f, ok := next.(*huh.Form); ok {
		m.f = f
	}
	return m, cmd
}

func (m *formModel) View() tea.View {
	if s := m.f.View(); s != "" {
		m.height = strings.Count(s, "\n") + 1
		return tea.NewView(s)
	}
	return tea.NewView(strings.Repeat("\n", max(m.height-1, 0)))
}

// line reads one answer, trimmed. A last line without a newline still
// counts; nothing at all is errNoAnswer.
func (p *prompter) line() (string, error) {
	s, err := p.in.ReadString('\n')
	if err != nil && s == "" {
		return "", errNoAnswer
	}
	return strings.TrimSpace(s), nil
}

// ask asks for a value, showing def, which an empty answer keeps.
func (p *prompter) ask(question, def string) (string, error) {
	if p.term != nil {
		var s string
		in := huh.NewInput().Title(question).Value(&s)
		if def != "" {
			in.Placeholder(def).Description("Enter keeps " + def)
		}
		err := p.form(in, func() string { return question }, func() string { return or(s, def) })
		return strings.TrimSpace(or(s, def)), err
	}
	hint := ""
	if def != "" {
		hint = "[" + def + "]"
	}
	p.question(question+":", hint)
	s, err := p.line()
	if err != nil {
		return "", err
	}
	if s == "" {
		return def, nil
	}
	return s, nil
}

// yesNo asks a yes/no question; an empty answer is def, anything else
// that is not a yes or a no asks again.
func (p *prompter) yesNo(question string, def bool) (bool, error) {
	if p.term != nil {
		v := def
		c := huh.NewConfirm().Title(question).Value(&v).Affirmative("Yes").Negative("No")
		err := p.form(c, func() string { return question }, func() string { return yesNoWord(v) })
		return v, err
	}
	hint := "[y/N]"
	if def {
		hint = "[Y/n]"
	}
	for {
		p.question(question, hint)
		s, err := p.line()
		if err != nil {
			return false, err
		}
		switch strings.ToLower(s) {
		case "":
			return def, nil
		case "y", "yes":
			return true, nil
		case "n", "no":
			return false, nil
		}
	}
}

func yesNoWord(v bool) string {
	if v {
		return "yes"
	}
	return "no"
}

// choose asks for one of options and returns its index; def is where it
// starts (the answer to an empty line). In lines, anything that is not an
// option's number asks again.
func (p *prompter) choose(question string, options []string, def int) (int, error) {
	if p.term != nil {
		v := def
		opts := make([]huh.Option[int], len(options))
		for i, o := range options {
			opts[i] = huh.NewOption(o, i)
		}
		s := huh.NewSelect[int]().Title(question).Options(opts...).Value(&v)
		err := p.form(s, func() string { return question }, func() string { return options[v] })
		return v, err
	}
	p.listQuestion(question)
	p.options(options)
	for {
		fmt.Fprintf(p.out, "    %s ", p.paint(dim, fmt.Sprintf("choose 1-%d [%d]:", len(options), def+1)))
		s, err := p.line()
		if err != nil {
			return 0, err
		}
		if s == "" {
			return def, nil
		}
		if n, err := strconv.Atoi(s); err == nil && n >= 1 && n <= len(options) {
			return n - 1, nil
		}
	}
}

// checklist asks which of items are in, starting from on. In lines, the
// list is shown with a box each, numbers toggle them, and an empty answer
// takes it as it stands; anything else asks again.
func (p *prompter) checklist(question string, items []string, on []bool) ([]bool, error) {
	on = append([]bool(nil), on...)
	if p.term != nil {
		var picked []int
		opts := make([]huh.Option[int], len(items))
		for i, it := range items {
			opts[i] = huh.NewOption(it, i).Selected(on[i])
		}
		m := huh.NewMultiSelect[int]().Title(question).Options(opts...).Value(&picked).Height(len(items) + 2)
		err := p.form(m, func() string { return question }, func() string {
			var names []string
			for _, i := range picked {
				// Without what is in parentheses: the list shows it.
				name, _, _ := strings.Cut(items[i], " (")
				names = append(names, name)
			}
			return or(strings.Join(names, ", "), "none")
		})
		for i := range on {
			on[i] = false
		}
		for _, i := range picked {
			on[i] = true
		}
		return on, err
	}
	p.listQuestion(question)
	for {
		boxed := make([]string, len(items))
		for i, it := range items {
			box := "[ ]"
			if on[i] {
				box = p.paint(green, "[x]")
			}
			boxed[i] = box + " " + it
		}
		p.options(boxed)
		fmt.Fprintf(p.out, "    %s ", p.paint(dim, "numbers to toggle, Enter to accept:"))
		s, err := p.line()
		if err != nil {
			return nil, err
		}
		if s == "" {
			return on, nil
		}
		next := append([]bool(nil), on...)
		ok := true
		for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ' ' || r == ',' }) {
			n, err := strconv.Atoi(f)
			if err != nil || n < 1 || n > len(items) {
				ok = false
				break
			}
			next[n-1] = !next[n-1]
		}
		if ok {
			on = next
		}
	}
}

// openTerminal is where setup reads its answers: the terminal, when stdin
// is one. Setup refuses without it rather than guess answers, or read them
// from a pipe that was meant for something else.
func (a *App) openTerminal() (io.ReadCloser, error) {
	if a.Terminal != nil {
		return a.Terminal()
	}
	if !tty.IsTerminal(os.Stdin.Fd()) {
		return nil, errors.New("stdin is not a terminal")
	}
	return os.Open("/dev/tty")
}
