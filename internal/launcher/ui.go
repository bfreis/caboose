package launcher

import (
	"fmt"
	"io"
	"os"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"

	"github.com/bfreis/caboose/internal/tty"
)

// How caboose looks where it talks to a person -- setup between its
// questions, doctor's report: a heading per step, prose wrapped to the
// terminal, and a mark on every outcome. Colors are written as they are and
// taken out again by colorWriter wherever out is no terminal, so a pipe, or
// a test, reads plain text.

// defaultWidth is where setup's prose wraps without a terminal to measure,
// and the widest it gets on one.
const defaultWidth = 80

// ui writes what caboose says to a person.
type ui struct {
	out io.Writer
	// width is the column prose wraps at; 0 wraps nothing.
	width int
	// step counts the headings, out of steps (0: not counted).
	step, steps int
}

// newUI writes to w, in colors when w takes them.
func newUI(w io.Writer, width int) *ui { return &ui{out: colorWriter(w), width: width} }

// termWidth is w's width, at most limit, when w is a terminal; else 0.
func termWidth(w io.Writer, limit int) int {
	f, ok := w.(*os.File)
	if !ok || !tty.IsTerminal(f.Fd()) {
		return 0
	}
	if cols := tty.Width(f.Fd()); cols > 0 {
		return min(cols, limit)
	}
	return limit
}

// colorWriter passes colors through to a terminal that takes them, and
// takes them out anywhere else (a pipe, a file, a test's buffer, NO_COLOR).
func colorWriter(w io.Writer) io.Writer {
	return colorprofile.NewWriter(w, os.Environ())
}

var (
	accent = lipgloss.NewStyle().Foreground(lipgloss.Color("#7571F9")).Bold(true)
	cyan   = lipgloss.NewStyle().Foreground(lipgloss.Color("#00B7EB"))
	green  = lipgloss.NewStyle().Foreground(lipgloss.Color("#02BF87"))
	yellow = lipgloss.NewStyle().Foreground(lipgloss.Color("#E8B339"))
	red    = lipgloss.NewStyle().Foreground(lipgloss.Color("#ED567A"))
	dim    = lipgloss.NewStyle().Faint(true)
	bold   = lipgloss.NewStyle().Bold(true)
)

// paint styles text line by line: lipgloss pads a block's lines to one
// width, which would leave trailing spaces.
func (p *ui) paint(s lipgloss.Style, text string) string {
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		if l != "" {
			lines[i] = s.Render(l)
		}
	}
	return strings.Join(lines, "\n")
}

// wrap fills text to p.width, the first line after first and the rest after
// rest; a newline in text starts a new line. With no width, each line of
// text is left whole.
func (p *ui) wrap(text, first, rest string) string {
	var b strings.Builder
	prefix := first
	for _, para := range strings.Split(text, "\n") {
		if p.width <= 0 {
			b.WriteString(prefix + para + "\n")
			prefix = rest
			continue
		}
		line, n, empty := prefix, lipgloss.Width(prefix), true
		for _, w := range strings.Fields(para) {
			ww := lipgloss.Width(w)
			if !empty && n+1+ww > p.width {
				b.WriteString(line + "\n")
				line, n, empty = rest, lipgloss.Width(rest), true
			}
			if !empty {
				line += " "
				n++
			}
			line += w
			n += ww
			empty = false
		}
		b.WriteString(line + "\n")
		prefix = rest
	}
	return b.String()
}

// banner opens a run.
func (p *ui) banner(title, sub string) {
	fmt.Fprintf(p.out, "\n  %s  %s\n", p.paint(accent, title), p.paint(dim, sub))
}

// heading starts a step: its title, counted when the run counts them, a
// rule, and what the step is about.
func (p *ui) heading(title, about string) {
	p.step++
	count := ""
	if p.steps > 0 {
		count = fmt.Sprintf("%d/%d", p.step, p.steps)
	}
	rule := max(p.width-4, 20)
	gap := max(rule-lipgloss.Width(title)-len(count), 1)
	fmt.Fprintf(p.out, "\n  %s%s%s\n", p.paint(accent, title), strings.Repeat(" ", gap), p.paint(dim, count))
	fmt.Fprintf(p.out, "  %s\n", p.paint(dim, strings.Repeat("─", rule)))
	if about != "" {
		fmt.Fprint(p.out, p.paint(dim, strings.TrimSuffix(p.wrap(about, "  ", "  "), "\n"))+"\n")
	}
	fmt.Fprintln(p.out)
}

// question writes q as a question in lines, followed by hint, leaving the
// cursor on its line for the answer.
func (p *ui) question(q, hint string) {
	s := strings.TrimSuffix(p.wrap(q, "  ? ", "    "), "\n")
	s = strings.Replace(s, "?", p.paint(cyan, "?"), 1)
	if hint != "" {
		s += " " + p.paint(dim, hint)
	}
	fmt.Fprint(p.out, s+" ")
}

// listQuestion writes q as a question whose answer is picked from a list
// below it.
func (p *ui) listQuestion(q string) {
	s := p.wrap(q, "  ? ", "    ")
	fmt.Fprint(p.out, strings.Replace(s, "?", p.paint(cyan, "?"), 1))
}

// answered records a form's answer, which the form itself does not leave.
func (p *ui) answered(q, a string) {
	fmt.Fprint(p.out, p.wrap(p.paint(dim, q)+" › "+p.paint(bold, a), "  "+p.paint(green, "✔")+" ", "    "))
}

// options lists a menu's choices, numbered from 1.
func (p *ui) options(opts []string) {
	for i, o := range opts {
		num := fmt.Sprintf("    %d  ", i+1)
		fmt.Fprint(p.out, p.wrap(o, p.paint(dim, num), strings.Repeat(" ", len(num))))
	}
}

// mark writes a line of prose after sym in style.
func (p *ui) mark(style lipgloss.Style, sym, format string, args ...any) {
	s := p.wrap(fmt.Sprintf(format, args...), "  "+sym+" ", "    ")
	fmt.Fprint(p.out, strings.Replace(s, sym, p.paint(style, sym), 1))
}

// ok is something done: written, built, started.
func (p *ui) ok(format string, args ...any) { p.mark(green, "✓", format, args...) }

// same is a step that changed nothing.
func (p *ui) same(format string, args ...any) {
	fmt.Fprint(p.out, p.paint(dim, strings.TrimSuffix(p.wrap(fmt.Sprintf(format, args...), "  · ", "    "), "\n"))+"\n")
}

// warn is something to know, or to do later.
func (p *ui) warn(format string, args ...any) { p.mark(yellow, "!", format, args...) }

// fail is something that did not work, or will not.
func (p *ui) fail(format string, args ...any) { p.mark(red, "✗", format, args...) }

// say is prose.
func (p *ui) say(format string, args ...any) {
	fmt.Fprint(p.out, p.wrap(fmt.Sprintf(format, args...), "  ", "  "))
}

// note is prose that matters less.
func (p *ui) note(format string, args ...any) {
	fmt.Fprint(p.out, p.paint(dim, strings.TrimSuffix(p.wrap(fmt.Sprintf(format, args...), "  ", "  "), "\n"))+"\n")
}

// bullets lists items, each after a bullet.
func (p *ui) bullets(items []string) {
	for _, it := range items {
		fmt.Fprint(p.out, p.wrap(it, "    "+p.paint(dim, "•")+" ", "      "))
	}
}

// table lists label/value pairs, the values aligned.
func (p *ui) table(rows [][2]string) {
	w := 0
	for _, r := range rows {
		w = max(w, lipgloss.Width(r[0]))
	}
	for _, r := range rows {
		pad := strings.Repeat(" ", w-lipgloss.Width(r[0])+2)
		fmt.Fprint(p.out, p.wrap(r[1], "    "+p.paint(dim, r[0])+pad, strings.Repeat(" ", w+6)))
	}
}

// diff shows lineDiff's output between from and to, colored.
func (p *ui) diff(from, to, d string) {
	fmt.Fprintf(p.out, "    %s\n    %s\n", p.paint(red, "--- "+from), p.paint(green, "+++ "+to))
	for _, l := range strings.Split(strings.TrimSuffix(d, "\n"), "\n") {
		switch {
		case strings.HasPrefix(l, "-"):
			l = p.paint(red, l)
		case strings.HasPrefix(l, "+"):
			l = p.paint(green, l)
		default:
			l = p.paint(dim, l)
		}
		fmt.Fprintf(p.out, "    %s\n", l)
	}
}

// mark is how a row of a checklist stands, shown before its label.
type mark struct {
	sym   string
	style lipgloss.Style
}

var (
	markOK        = mark{"✓", green}
	markNote      = mark{"!", yellow}
	markProblem   = mark{"✗", red}
	markUnchecked = mark{"–", dim}
)

// row writes a checklist row: m, label padded to lw, then text, which
// wraps under itself.
func (p *ui) row(m mark, label string, lw int, text string) {
	first := "  " + p.paint(m.style, m.sym) + " " + label + strings.Repeat(" ", max(lw-len(label), 0)) + "  "
	fmt.Fprint(p.out, p.wrap(text, first, strings.Repeat(" ", lw+6)))
}

// blank is an empty line.
func (p *ui) blank() { fmt.Fprintln(p.out) }

// short is path with the home directory as ~, for saying.
func (a *App) short(path string) string {
	home := a.Cfg.Home
	if home == "" || home == "/" {
		return path
	}
	if path == home {
		return "~"
	}
	if strings.HasPrefix(path, home+"/") {
		return "~" + path[len(home):]
	}
	return path
}

// code is a command or a key, set apart in prose.
func (p *ui) code(s string) string { return p.paint(bold, s) }

// capFirst capitalizes s's first letter, for a title written mid-sentence.
func capFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
