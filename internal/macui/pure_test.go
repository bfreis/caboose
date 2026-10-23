package macui

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/bfreis/caboose/internal/menubar"
)

func TestApproved(t *testing.T) {
	for _, c := range []struct {
		code int
		want bool
	}{
		{secondButton, true},
		{1001, true},
		{firstButton, false},
		{1000, false},
		{responseStop, false},
		{responseAbort, false},
		{-1001, false},
		{-1000, false},
		{0, false},
		{1, false},
		{1002, false},
		{-1, false},
	} {
		if got := approved(c.code); got != c.want {
			t.Errorf("approved(%d) = %v, want %v", c.code, got, c.want)
		}
	}
}

func TestConfirmTextOrder(t *testing.T) {
	c := menubar.Confirmation{
		ID:           "c1",
		Message:      "Allow #12 for 8 hours?",
		Effect:       "Runs the alias deploy on the host.",
		AnswerEffect: "Allows it until 18:00.",
		SandboxSays:  menubar.SandboxLabel + "\nplease\nRecommends: yes",
		Action:       "Allow for 8 hours",
	}
	a := confirmText(c)
	if a.Message != c.Message || a.Action != c.Action {
		t.Fatalf("message/action = %q / %q", a.Message, a.Action)
	}
	want := "Runs the alias deploy on the host.\n\nAllows it until 18:00.\n\n" +
		menubar.SandboxLabel + "\nplease\nRecommends: yes"
	if a.Informative != want {
		t.Fatalf("informative =\n%q\nwant\n%q", a.Informative, want)
	}
	// The host's text always comes before the sandbox's.
	ie, ia, is := strings.Index(a.Informative, c.Effect), strings.Index(a.Informative, c.AnswerEffect), strings.Index(a.Informative, menubar.SandboxLabel)
	if !(ie >= 0 && ie < ia && ia < is) {
		t.Fatalf("order: effect %d, answer %d, sandbox %d", ie, ia, is)
	}
}

func TestConfirmTextEmptyParts(t *testing.T) {
	a := confirmText(menubar.Confirmation{Message: "Stop dev?", Effect: "Ends every session."})
	if a.Informative != "Ends every session." {
		t.Fatalf("informative = %q", a.Informative)
	}
	if a.Action != confirmTitle {
		t.Fatalf("action = %q", a.Action)
	}
}

func TestSandboxBlockLabelsAndClips(t *testing.T) {
	// The label is added when the core's text lacks it.
	if got := sandboxBlock("hello"); got != menubar.SandboxLabel+"\nhello" {
		t.Fatalf("sandboxBlock = %q", got)
	}
	if got := sandboxBlock("  "); got != "" {
		t.Fatalf("empty = %q", got)
	}
	if got := sandboxBlock(menubar.SandboxLabel); got != "" {
		t.Fatalf("label alone = %q", got)
	}
	long := sandboxBlock(strings.Repeat("x", 5000))
	if n := utf8.RuneCountInString(long); n != menubar.MaxSandboxSays {
		t.Fatalf("clipped to %d runes, want %d", n, menubar.MaxSandboxSays)
	}
	if !strings.HasPrefix(long, menubar.SandboxLabel) || !strings.HasSuffix(long, "…") {
		t.Fatalf("clipped block = %q...", long[:60])
	}
	// Bidi overrides and controls are not shown; line breaks are kept.
	got := sandboxBlock("a\u202eb\x07c\r\nd")
	if got != menubar.SandboxLabel+"\na?b?c\nd" {
		t.Fatalf("cleaned = %q", got)
	}
}

func TestURLText(t *testing.T) {
	a := urlText(menubar.URLDialog{ID: "u", Env: "dev", URL: "https://example.com/x", SandboxSays: "docs"})
	if a.Message != "dev asks to open a URL" || a.Action != openTitle {
		t.Fatalf("got %+v", a)
	}
	if a.Informative != "https://example.com/x\n\n"+menubar.SandboxLabel+"\ndocs" {
		t.Fatalf("informative = %q", a.Informative)
	}
	if a := urlText(menubar.URLDialog{URL: "https://e.com"}); a.Message != "A sandbox asks to open a URL" || a.Informative != "https://e.com" {
		t.Fatalf("got %+v", a)
	}
}

func TestInfoTextAndBounds(t *testing.T) {
	a := infoText(strings.Repeat("y", 10000))
	if a.Action != "" || utf8.RuneCountInString(a.Informative) != maxInformative {
		t.Fatalf("info: action %q, %d runes", a.Action, utf8.RuneCountInString(a.Informative))
	}
	m := confirmText(menubar.Confirmation{Message: strings.Repeat("m", 500), Action: strings.Repeat("a", 500)})
	if utf8.RuneCountInString(m.Message) > 2*menubar.MaxTitle || utf8.RuneCountInString(m.Action) > menubar.MaxTitle {
		t.Fatalf("message %d, action %d runes", utf8.RuneCountInString(m.Message), utf8.RuneCountInString(m.Action))
	}
}

func TestCText(t *testing.T) {
	if got := cText("a\x00b"); got != "a?b" {
		t.Fatalf("cText = %q", got)
	}
}

func TestFlatten(t *testing.T) {
	m := menubar.Menu{Items: []menubar.Item{
		{ID: "a", Title: "A", Enabled: true},
		{Separator: true, Title: "ignored"},
		{Title: "info line", Info: true, Enabled: true},
		{Title: "Env", Enabled: true, Children: []menubar.Item{
			{ID: "e1", Title: "Stop", Enabled: true, Checked: true},
			{Title: "More", Enabled: true, Children: []menubar.Item{{ID: "deep", Title: "Deep", Enabled: true}}},
		}},
		{ID: "q", Title: strings.Repeat("t", 200) + "\u202e", Enabled: false},
	}}
	got := flatten(m)
	want := []flatItem{
		{Parent: -1, Title: "A", ID: "a", Enabled: true},
		{Parent: -1, Separator: true},
		{Parent: -1, Title: "info line"},
		{Parent: -1, Title: "Env", Enabled: true, Submenu: true},
		{Parent: 3, Title: "Stop", ID: "e1", Enabled: true, Checked: true},
		{Parent: 3, Title: "More", Enabled: true, Submenu: true},
		{Parent: 5, Title: "Deep", ID: "deep", Enabled: true},
		{Parent: -1, Title: menubar.Clip(strings.Repeat("t", 200), menubar.MaxTitle), ID: "q"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d items, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("item %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	for i, it := range got {
		if it.Parent >= i || (it.Parent >= 0 && !got[it.Parent].Submenu) {
			t.Errorf("item %d's parent %d is not an earlier submenu", i, it.Parent)
		}
		if utf8.RuneCountInString(it.Title) > menubar.MaxTitle {
			t.Errorf("item %d title too long", i)
		}
	}
}

func TestFlattenBounds(t *testing.T) {
	// Depth past maxMenuDepth is cut.
	var deep menubar.Item
	cur := &deep
	for i := 0; i < 10; i++ {
		cur.Title = "lvl"
		cur.Children = []menubar.Item{{}}
		cur = &cur.Children[0]
	}
	got := flatten(menubar.Menu{Items: []menubar.Item{deep}})
	if len(got) != maxMenuDepth || got[len(got)-1].Submenu {
		t.Fatalf("depth: %d items, last %+v", len(got), got[len(got)-1])
	}
	many := make([]menubar.Item, maxMenuItems+50)
	if n := len(flatten(menubar.Menu{Items: many})); n != maxMenuItems {
		t.Fatalf("items: %d", n)
	}
}

// fakeEvents records what the glue posts.
type fakeEvents struct{ got []string }

func (f *fakeEvents) Selected(id string) { f.got = append(f.got, "selected "+id) }
func (f *fakeEvents) MenuOpened()        { f.got = append(f.got, "opened") }
func (f *fakeEvents) MenuClosed()        { f.got = append(f.got, "closed") }
func (f *fakeEvents) Confirmed(id string, ok bool) {
	f.got = append(f.got, "confirmed "+id+" "+map[bool]string{true: "yes", false: "no"}[ok])
}
func (f *fakeEvents) URLAnswered(id string, open bool) {
	f.got = append(f.got, "url "+id+" "+map[bool]string{true: "yes", false: "no"}[open])
}
func (f *fakeEvents) NotificationClicked(id string) { f.got = append(f.got, "clicked "+id) }
func (f *fakeEvents) NotificationAuth(g bool)       { f.got = append(f.got, fmt.Sprint("auth ", g)) }
func (f *fakeEvents) AlertClosed(id string)         { f.got = append(f.got, "alert closed "+id) }

func (f *fakeEvents) take() string {
	s := strings.Join(f.got, ", ")
	f.got = nil
	return s
}

func TestAlertQueue(t *testing.T) {
	var q alertQueue
	ev := &fakeEvents{}
	q.push(&alertReq{kind: alertConfirm, id: "c"})
	q.push(&alertReq{kind: alertURL, id: "u"})
	q.push(infoReq("i", "hello"))

	r := q.next()
	if r == nil || r.id != "c" || !q.showing(r.gen) {
		t.Fatalf("first = %+v", r)
	}
	if q.next() != nil {
		t.Fatal("a second alert while one is shown")
	}
	q.done(secondButton, ev)
	if got := ev.take(); got != "confirmed c yes" {
		t.Fatalf("got %q", got)
	}
	if q.showing(r.gen) {
		t.Fatal("still showing after done")
	}

	r = q.next()
	if r.id != "u" {
		t.Fatalf("second = %+v", r)
	}
	q.done(responseAbort, ev)
	if got := ev.take(); got != "url u no" {
		t.Fatalf("got %q", got)
	}
	q.next()
	q.done(responseStop, ev) // info answers nothing but its closing
	if got := ev.take(); got != "alert closed i" {
		t.Fatalf("info posted %q", got)
	}
	if q.next() != nil {
		t.Fatal("queue not empty")
	}
}

func TestAlertReqs(t *testing.T) {
	c := confirmReq(menubar.Confirmation{ID: "c", Message: "Stop dev?", Action: "Stop"})
	u := urlReq(menubar.URLDialog{ID: "u", Env: "dev", URL: "https://e.com"})
	i := infoReq("i", "hello")
	if c.kind != alertConfirm || c.id != "c" || c.text.Action != "Stop" {
		t.Errorf("confirm = %+v", c)
	}
	if u.kind != alertURL || u.id != "u" || u.text.Action != openTitle {
		t.Errorf("url = %+v", u)
	}
	if i.kind != alertInfo || i.id != "i" || i.text.Action != "" || i.text.Informative != "hello" {
		t.Errorf("info = %+v", i)
	}
}

func TestAlertQueueCancelAndOthers(t *testing.T) {
	for _, code := range []int{firstButton, responseStop, responseAbort, 0, 7} {
		var q alertQueue
		ev := &fakeEvents{}
		q.push(&alertReq{kind: alertConfirm, id: "c"})
		q.next()
		q.done(code, ev)
		if got := ev.take(); got != "confirmed c no" {
			t.Fatalf("code %d: %q", code, got)
		}
	}
}

func TestAlertQueueDismiss(t *testing.T) {
	var q alertQueue
	ev := &fakeEvents{}
	q.push(&alertReq{kind: alertConfirm, id: "a"})
	q.push(&alertReq{kind: alertURL, id: "b"})
	q.push(&alertReq{kind: alertURL, id: "c"})
	r := q.next()
	if q.dismiss("b") {
		t.Fatal("dismissing a waiting alert asked for an abort")
	}
	if !q.dismiss("a") {
		t.Fatal("dismissing the shown alert did not ask for an abort")
	}
	if q.dismiss("a") {
		t.Fatal("a second dismiss asked again")
	}
	q.done(responseAbort, ev)
	if got := ev.take(); got != "" {
		t.Fatalf("a dismissed alert posted %q", got)
	}
	if q.showing(r.gen) {
		t.Fatal("still showing")
	}
	// Even a stray approving result posts nothing for a dismissed alert.
	r = q.next()
	if r.id != "c" {
		t.Fatalf("next = %+v", r)
	}
	q.dismiss("c")
	q.done(secondButton, ev)
	if got := ev.take(); got != "" {
		t.Fatalf("a dismissed alert posted %q", got)
	}
}

func TestAlertQueueQuit(t *testing.T) {
	var q alertQueue
	ev := &fakeEvents{}
	q.push(&alertReq{kind: alertConfirm, id: "a"})
	q.push(&alertReq{kind: alertConfirm, id: "b"})
	q.next()
	if !q.quit() {
		t.Fatal("quit with an alert shown did not ask for an abort")
	}
	q.done(secondButton, ev)
	if got := ev.take(); got != "" {
		t.Fatalf("posted %q after quit", got)
	}
	q.push(&alertReq{kind: alertConfirm, id: "c"})
	if r := q.next(); r != nil {
		t.Fatalf("an alert after quit: %+v", r)
	}
	// An informational alert Quit closes posts no closing either: the
	// app is ending already.
	var info alertQueue
	info.push(infoReq("busy", "already running"))
	info.next()
	if !info.quit() {
		t.Fatal("quit with an info alert shown did not ask for an abort")
	}
	info.done(responseAbort, ev)
	if got := ev.take(); got != "" {
		t.Fatalf("an info alert closed by quit posted %q", got)
	}
	var idle alertQueue
	if idle.quit() {
		t.Fatal("quit with no alert asked for an abort")
	}
}

// A URL dialog shown when the app quits closes with no answer, whatever
// runModal returns (the abort, or a click on Open racing it): the daemon
// denies the request in flight once the app's attachment ends.
func TestAlertQueueQuitURLDialog(t *testing.T) {
	for _, code := range []int{responseAbort, secondButton} {
		var q alertQueue
		ev := &fakeEvents{}
		q.push(urlReq(menubar.URLDialog{ID: "u", Env: "dev", URL: "https://example.com/"}))
		q.push(urlReq(menubar.URLDialog{ID: "v", Env: "dev", URL: "https://example.org/"}))
		if r := q.next(); r == nil || r.id != "u" {
			t.Fatalf("next = %+v", r)
		}
		if !q.quit() {
			t.Fatal("quit with a URL dialog shown did not ask for an abort")
		}
		q.done(code, ev)
		if got := ev.take(); got != "" {
			t.Fatalf("runModal's %d after quit posted %q", code, got)
		}
		if r := q.next(); r != nil {
			t.Fatalf("the waiting dialog shown after quit: %+v", r)
		}
	}
}

func TestRegistry(t *testing.T) {
	var r registry
	n := 0
	a := r.put(func() { n++ })
	b := r.put(func() { n += 10 })
	if a == 0 || b == 0 || a == b {
		t.Fatalf("numbers %d, %d", a, b)
	}
	r.take(b)()
	r.take(a)()
	if n != 11 {
		t.Fatalf("n = %d", n)
	}
	if r.take(a) != nil || r.take(12345) != nil {
		t.Fatal("taken twice")
	}
}

func TestSink(t *testing.T) {
	var s sink
	s.selected("x") // nothing to post to: no panic
	s.menuOpened()
	ev := &fakeEvents{}
	s.set(ev)
	s.selected("x")
	s.selected("")
	s.menuOpened()
	s.menuClosed()
	if got := ev.take(); got != "selected x, opened, closed" {
		t.Fatalf("got %q", got)
	}
	s.set(nil)
	s.selected("y")
	if got := ev.take(); got != "" {
		t.Fatalf("posted after reset: %q", got)
	}
}

func TestErrMacOnly(t *testing.T) {
	if !strings.Contains(ErrMacOnly.Error(), "macOS only") {
		t.Fatal(ErrMacOnly)
	}
}

func TestNormEncoding(t *testing.T) {
	for in, want := range map[string]string{
		"@24@0:8r*16":          "@@:*",
		"Vv16@0:8":             "v@:",
		"c24@0:8q16":           "B@:q",
		"B24@0:8q16":           "B@:q",
		"v24@0:8@\"NSMenu\"16": "v@:@",
		"@80@0:8Q16{CGPoint=dd}24Q40d48q56@64s72q76q84": "@@:Q{CGPoint=dd}Qdq@sqq",
		"v32@0:8{CGRect={CGPoint=dd}{CGSize=dd}}16":     "v@:{CGRect={CGPoint=dd}{CGSize=dd}}",
		"v24@0:8^{opaqueRon=}16":                        "v@:^{opaqueRon=}",
		"v32@0:8@16@?24":                                "v@:@@?",
		"v32@0:8@16@?<v@?Q>24":                          "v@:@@?",
		"v32@0:8@\"UNNotification\"16@?<v@?Q>24":        "v@:@@?",
	} {
		if got := normEncoding(in); got != want {
			t.Errorf("normEncoding(%q) = %q, want %q", in, got, want)
		}
	}
}

// fakeLookup is a runtime with what the test gives it.
type fakeLookup struct {
	classes map[string]bool
	methods map[string]string // "Class -name" or "Class +name" → encoding ("" for none)
	protos  map[string]string // "Proto name" → encoding
}

func fullLookup() *fakeLookup {
	f := &fakeLookup{classes: map[string]bool{}, methods: map[string]string{}, protos: map[string]string{}}
	for _, c := range classTable {
		f.classes[c] = true
	}
	for _, e := range selTable {
		// What the runtime gives: offsets and all.
		f.methods[e.Class+" "+methodName(e)] = withOffsets(e.Types)
		if e.Protocol != "" {
			f.protos[e.Protocol+" "+e.Name] = withOffsets(e.Types)
		}
	}
	return f
}

// withOffsets puts made-up frame offsets after each type, as the runtime
// writes them.
func withOffsets(enc string) string {
	var b strings.Builder
	for i := 0; i < len(enc); {
		j := skipType(enc, i)
		b.WriteString(enc[i:j])
		b.WriteString("8")
		i = j
	}
	return b.String()
}

func (f *fakeLookup) HasClass(c string) bool { return f.classes[c] }
func (f *fakeLookup) Responds(c, s string, meta bool) bool {
	_, ok := f.methods[c+" "+methodName(selEntry{Name: s, Meta: meta})]
	return ok
}
func (f *fakeLookup) Encoding(c, s string, meta bool) (string, bool) {
	e, ok := f.methods[c+" "+methodName(selEntry{Name: s, Meta: meta})]
	return e, ok && e != ""
}
func (f *fakeLookup) ProtocolEncoding(p, s string) (string, bool) {
	e, ok := f.protos[p+" "+s]
	return e, ok
}

func TestCheckAllPresent(t *testing.T) {
	var out bytes.Buffer
	if err := check(fullLookup(), &out); err != nil {
		t.Fatalf("check: %v\n%s", err, out.String())
	}
	if !strings.HasPrefix(out.String(), "ok: ") || strings.Count(out.String(), "\n") != 1 {
		t.Fatalf("output %q", out.String())
	}
}

func TestCheckFindsProblems(t *testing.T) {
	f := fullLookup()
	delete(f.classes, "NSAlert")
	delete(f.methods, "NSMenu -addItem:")
	f.methods["NSStatusBar -statusItemWithLength:"] = "@24@0:8{CGSize=dd}16" // a struct where a double was meant
	f.methods["NSMenuItem -setEnabled:"] = ""                                // answers, but no encoding
	f.protos["NSMenuDelegate menuDidClose:"] = "v24@0:8q16"
	delete(f.protos, "NSMenuDelegate menuWillOpen:")
	var out bytes.Buffer
	err := check(f, &out)
	if err == nil {
		t.Fatalf("no error; output %q", out.String())
	}
	for _, want := range []string{
		"missing class NSAlert",
		"missing selector NSMenu -addItem:",
		"type encoding of NSStatusBar -statusItemWithLength:",
		"no type encoding for NSMenuItem -setEnabled:",
		"type encoding of menuDidClose: in NSMenuDelegate",
		"protocol NSMenuDelegate declares no menuWillOpen:",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "NSAlert -") {
		t.Errorf("selectors of a missing class reported too:\n%s", out.String())
	}
	if strings.HasPrefix(out.String(), "ok") {
		t.Error("ok with problems")
	}
}

// TestTableConsistent holds the table to itself: every selector's class is
// in the class table, no entry is listed twice, and every encoding
// declares as many arguments as the selector takes.
func TestTableConsistent(t *testing.T) {
	classes := map[string]bool{}
	for _, c := range classTable {
		if classes[c] {
			t.Errorf("class %s listed twice", c)
		}
		classes[c] = true
	}
	seen := map[string]bool{}
	for _, e := range selTable {
		k := e.Class + " " + methodName(e)
		if seen[k] {
			t.Errorf("%s listed twice", k)
		}
		seen[k] = true
		if !classes[e.Class] {
			t.Errorf("%s: class not in the class table", k)
		}
		if e.Types == "" {
			continue
		}
		if normEncoding(e.Types) != e.Types {
			t.Errorf("%s: encoding %q is not normalized", k, e.Types)
		}
		if got, want := encodingArgs(e.Types), strings.Count(e.Name, ":"); got != want {
			t.Errorf("%s: encoding %q has %d arguments, the selector %d", k, e.Types, got, want)
		}
		if j := skipType(e.Types, 0); j < 0 || !strings.HasPrefix(e.Types[j:], "@:") {
			t.Errorf("%s: encoding %q lacks self and _cmd", k, e.Types)
		}
	}
}

func TestMenuHold(t *testing.T) {
	var h menuHold
	a := menubar.Menu{Items: []menubar.Item{{ID: "a"}}}
	b := menubar.Menu{Items: []menubar.Item{{ID: "b"}}}
	c := menubar.Menu{Items: []menubar.Item{{ID: "c"}}}
	if !h.set(a) {
		t.Fatal("a closed menu's tree was held")
	}
	if _, ok := h.closed(); ok {
		t.Fatal("a tree came out of an empty hold")
	}
	h.opened()
	if h.set(b) || h.set(c) {
		t.Fatal("a tree was applied while the menu was open")
	}
	m, ok := h.closed()
	if !ok || m.Items[0].ID != "c" {
		t.Fatalf("closed gave %+v, %v; want the newest tree", m, ok)
	}
	if _, ok := h.closed(); ok {
		t.Fatal("the held tree came out twice")
	}
	if !h.set(a) {
		t.Fatal("held after the menu closed")
	}
}

func TestEnableIfShowing(t *testing.T) {
	var q alertQueue
	q.push(&alertReq{kind: alertConfirm, id: "a"})
	q.push(&alertReq{kind: alertConfirm, id: "b"})
	first := q.next()
	enabled := ""
	q.enableIfShowing(first.gen, func() { enabled += "a" })
	q.done(firstButton, nil)
	second := q.next()
	// The first alert's timer, late: it must not touch the second's button.
	q.enableIfShowing(first.gen, func() { enabled += "stale" })
	q.enableIfShowing(second.gen, func() { enabled += "b" })
	q.done(firstButton, nil)
	q.enableIfShowing(second.gen, func() { enabled += "gone" })
	if enabled != "ab" {
		t.Fatalf("enabled %q, want %q", enabled, "ab")
	}
}

func TestDismissNeverClosesInfoOrEmpty(t *testing.T) {
	var q alertQueue
	q.push(infoReq("1", "one"))
	q.push(infoReq("2", "two"))
	q.push(&alertReq{kind: alertConfirm, id: ""})
	q.next()
	if q.dismiss("") {
		t.Fatal("Dismiss(\"\") aborted an info alert")
	}
	if len(q.pending) != 2 {
		t.Fatalf("Dismiss(\"\") dropped waiting alerts: %d left", len(q.pending))
	}
	var q2 alertQueue
	q2.push(&alertReq{kind: alertInfo, id: "x"})
	q2.next()
	if q2.dismiss("x") {
		t.Fatal("Dismiss aborted an info alert by its id")
	}
}

// encodingArgs is how many arguments a normalized encoding declares
// after self and _cmd, -1 when it cannot be read.
func encodingArgs(enc string) int {
	n := 0
	for i := 0; i < len(enc); {
		j := skipType(enc, i)
		if j <= i {
			return -1
		}
		n++
		i = j
	}
	return n - 3 // the return type, self and _cmd
}

// skipType returns the index after the type that starts at i.
func skipType(enc string, i int) int {
	if i >= len(enc) {
		return i
	}
	switch enc[i] {
	case '^':
		return skipType(enc, i+1)
	case '{', '(':
		open, close := enc[i], byte('}')
		if open == '(' {
			close = ')'
		}
		depth := 0
		for j := i; j < len(enc); j++ {
			switch enc[j] {
			case open:
				depth++
			case close:
				depth--
				if depth == 0 {
					return j + 1
				}
			}
		}
		return -1
	case '@':
		if i+1 < len(enc) && enc[i+1] == '?' {
			return i + 2 // a block
		}
	case '[':
		if j := strings.IndexByte(enc[i:], ']'); j >= 0 {
			return i + j + 1
		}
		return -1
	}
	return i + 1
}
