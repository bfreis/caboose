package launcher

// doctorLabelWidth is the widest of doctor's labels ("container", "ssh
// agent"): rows printed as they come cannot wait to measure.
const doctorLabelWidth = 9

// liveReport prints doctor's findings on a terminal as they come, above a
// spinner that says what is being checked: the checks ask docker, the
// container and maybe a remote, and a report that only showed up at the
// end would look stuck until then.
type liveReport struct {
	*spinner
	home string
	lw   int
	prev string // the label of the row printed last
}

func startLive(u *ui, home string, lw int) *liveReport {
	return &liveReport{spinner: startSpinner(u, "checking"), home: home, lw: lw}
}

// checking says what is being checked now.
func (l *liveReport) checking(what string) { l.set("checking " + what) }

// row prints r above the spinner.
func (l *liveReport) row(r finding) {
	l.above(func() {
		renderRow(l.u, r, l.lw, l.prev, l.home)
		l.prev = r.label
	})
}
