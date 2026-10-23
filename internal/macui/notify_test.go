package macui

import (
	"os"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"unsafe"

	"github.com/bfreis/caboose/internal/menubar"
)

func TestBlockLayout(t *testing.T) {
	var l blockLiteral
	var d blockDescriptor
	if unsafe.Sizeof(uintptr(0)) != 8 {
		t.Skip("the layout is checked for 64-bit systems")
	}
	for name, c := range map[string][2]uintptr{
		"isa":        {unsafe.Offsetof(l.isa), 0},
		"flags":      {unsafe.Offsetof(l.flags), 8},
		"invoke":     {unsafe.Offsetof(l.invoke), blockInvokeOffset},
		"descriptor": {unsafe.Offsetof(l.descriptor), 24},
		"size":       {unsafe.Sizeof(l), blockLiteralSize},
		"desc.size":  {unsafe.Offsetof(d.size), 8},
		"signature":  {unsafe.Offsetof(d.signature), 16},
		"desc total": {unsafe.Sizeof(d), blockDescSize},
	} {
		if c[0] != c[1] {
			t.Errorf("%s at %d, want %d", name, c[0], c[1])
		}
	}
	if blockIsGlobal != 0x10000000 || blockHasSignature != 0x40000000 {
		t.Errorf("flags %#x %#x", blockIsGlobal, blockHasSignature)
	}
}

func TestInitGlobalBlockAndReadInvoke(t *testing.T) {
	var l blockLiteral
	var d blockDescriptor
	sig := authSignature
	initGlobalBlock(&l, &d, 0x1111, 0x2222, &sig[0])
	if l.isa != 0x1111 || l.invoke != 0x2222 {
		t.Fatalf("%+v", l)
	}
	if l.flags&blockIsGlobal == 0 || l.flags&blockHasSignature == 0 {
		t.Errorf("flags %#x", l.flags)
	}
	if l.descriptor != uintptr(unsafe.Pointer(&d)) || d.size != blockLiteralSize || d.signature != &sig[0] {
		t.Errorf("descriptor %+v", d)
	}
	if got := readInvoke(uintptr(unsafe.Pointer(&l))); got != 0x2222 {
		t.Errorf("readInvoke = %#x", got)
	}
	runtime.KeepAlive(&l)
	if readInvoke(0) != 0 {
		t.Error("nil block has an invoke")
	}
}

func TestAuthSignature(t *testing.T) {
	if string(authSignature[:len(authSignature)-1]) != "v@?B@" || authSignature[len(authSignature)-1] != 0 {
		t.Errorf("%q", authSignature)
	}
}

func TestNotificationOptions(t *testing.T) {
	if authOptionAlert != 4 || presentBanner != 16 || presentList != 8 || presentAll != 24 {
		t.Errorf("%d %d %d %d", authOptionAlert, presentBanner, presentList, presentAll)
	}
}

func TestBoolArg(t *testing.T) {
	for r, want := range map[uintptr]bool{0: false, 1: true, 0x100: false, 0xdeadbe01: true, 0xdeadbe00: false} {
		if got := boolArg(r); got != want {
			t.Errorf("boolArg(%#x) = %v", r, got)
		}
	}
}

func TestLoginStateFromStatus(t *testing.T) {
	for st, want := range map[int]menubar.LoginState{
		0: menubar.LoginOff,
		1: menubar.LoginOn,
		2: menubar.LoginNeedsApproval,
		3: menubar.LoginOff,
		4: menubar.LoginUnavailable, -1: menubar.LoginUnavailable, 99: menubar.LoginUnavailable,
	} {
		if got := loginStateFromStatus(st); got != want {
			t.Errorf("status %d = %v, want %v", st, got, want)
		}
	}
}

func TestLoginError(t *testing.T) {
	if got := loginError(true, "Operation not permitted").Error(); got != "could not register the login item: Operation not permitted" {
		t.Error(got)
	}
	if got := loginError(false, "").Error(); got != "could not unregister the login item" {
		t.Error(got)
	}
	got := loginError(true, "bad\x1b[31m\n"+strings.Repeat("x", 1000)).Error()
	if strings.ContainsAny(got, "\x1b\n") || len([]rune(got)) > 60+maxLoginError {
		t.Errorf("not bounded or printable: %q", got)
	}
}

func TestClipNotification(t *testing.T) {
	n := clipNotification(menubar.Notification{ID: "a", Title: "T\x07" + strings.Repeat("t", 500), Body: strings.Repeat("b", 900) + "\n"})
	if n.ID != "a" || len([]rune(n.Title)) != maxNotifTitle || len([]rune(n.Body)) != maxNotifBody || strings.Contains(n.Title, "\x07") {
		t.Errorf("%q %d %d", n.ID, len([]rune(n.Title)), len([]rune(n.Body)))
	}
}

func TestSinkNotifications(t *testing.T) {
	var s sink
	s.notificationClicked("x") // nothing to post to
	s.notificationAuth(true)
	ev := &fakeEvents{}
	s.set(ev)
	s.notificationClicked("env/a")
	s.notificationClicked("")
	s.notificationAuth(false)
	s.notificationAuth(true)
	if got := ev.take(); got != "clicked env/a, auth false, auth true" {
		t.Errorf("got %q", got)
	}
}

func TestNotificationID(t *testing.T) {
	for id, want := range map[string]string{
		"n12":                   "n12",
		caboose_summary:         caboose_summary,
		"":                      "",
		"a b":                   "",
		"\xff":                  "",
		"é":                     "",
		strings.Repeat("x", 64): strings.Repeat("x", 64),
		strings.Repeat("x", 65): "",
	} {
		if got := notificationID(id); got != want {
			t.Errorf("notificationID(%q) = %q, want %q", id, got, want)
		}
	}
	if n := clipNotification(menubar.Notification{ID: "\xff", Title: "t"}); n.ID != "" {
		t.Errorf("an invalid identifier kept: %q", n.ID)
	}
}

const caboose_summary = menubar.SummaryID

// A delegate method a class registers under a protocol must carry that
// protocol in the self-check table, so the self-check holds its encoding
// to the protocol's declaration. Every method of the notification
// delegate is one; the menu target's are menuWillOpen: and menuDidClose:
// (itemSelected: is its own action, in no protocol).
func TestProtocolMethodsInTable(t *testing.T) {
	delegates := map[string]bool{"menuWillOpen:": true, "menuDidClose:": true}
	re := regexp.MustCompile(`(?s)registerClass\((\w+), "(\w+)", \[\]method\{(.*?)\n\t*\}\)`)
	selRe := regexp.MustCompile(`sel\("([^"]+)"\)`)
	found := 0
	for _, f := range []string{"app_darwin.go", "notify_darwin.go"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range re.FindAllStringSubmatch(string(b), -1) {
			for _, s := range selRe.FindAllStringSubmatch(m[3], -1) {
				if m[1] != "notificationDelegateClass" && !delegates[s[1]] {
					continue
				}
				found++
				ok := false
				for _, e := range selTable {
					if e.Name == s[1] && e.Protocol == m[2] {
						ok = true
					}
				}
				if !ok {
					t.Errorf("%s: %s registers %s under %s, but its table entry does not name that protocol", f, m[1], s[1], m[2])
				}
			}
		}
	}
	if found != 4 {
		t.Fatalf("found %d delegate methods, want 4; the scan no longer matches registerClass", found)
	}
}

// The click handler must call the system's completion handler on every
// path: a deferred call, as the first statement.
func TestDidReceiveAlwaysCompletes(t *testing.T) {
	b, err := os.ReadFile("notify_darwin.go")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`func didReceive\(response, handler uintptr\) \{\n\tdefer invokeBlock\(handler\)\n`).Match(b) {
		t.Error("didReceive must start with defer invokeBlock(handler)")
	}
}

// registeredMethods reads every method the glue registers: its class's
// constant, selector, encoding and implementation, as written.
func registeredMethods(t *testing.T) [][4]string {
	t.Helper()
	re := regexp.MustCompile(`(?s)registerClass\((\w+), "\w*", \[\]method\{(.*?)\n\t*\}\)`)
	line := regexp.MustCompile(`\{sel\("([^"]+)"\), "([^"]*)", (\w+)\}`)
	var out [][4]string
	for _, f := range []string{"app_darwin.go", "notify_darwin.go"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range re.FindAllStringSubmatch(string(b), -1) {
			for _, l := range line.FindAllStringSubmatch(m[2], -1) {
				out = append(out, [4]string{m[1], l[1], l[2], l[3]})
			}
		}
	}
	return out
}

// Every method a registered class implements is registered with the
// encoding its table entry gives, which the self-check then holds the
// running class to.
func TestRegisteredMethodsMatchTable(t *testing.T) {
	classes := map[string]string{"menuTargetClass": menuTargetClass, "alertTargetClass": alertTargetClass,
		"notificationDelegateClass": notificationDelegateClass}
	ms := registeredMethods(t)
	if len(ms) != 7 {
		t.Fatalf("found %d registered methods, want 7; the scan no longer matches registerClass", len(ms))
	}
	for _, m := range ms {
		cls, ok := classes[m[0]]
		if !ok {
			t.Errorf("%s registered on an unknown class %s", m[1], m[0])
			continue
		}
		found := false
		for _, e := range selTable {
			if e.Class == cls && e.Name == m[1] && !e.Meta {
				found = true
				if e.Types != m[2] {
					t.Errorf("%s %s registered as %q, its table entry says %q", cls, m[1], m[2], e.Types)
				}
			}
		}
		if !found {
			t.Errorf("%s %s registered but not in the table", cls, m[1])
		}
	}
}

// While an alert is modal, AppKit drops a menu item's action unless its
// target answers worksWhenModal with YES: without it, Quit did nothing
// while a URL dialog was up. The menu target answers YES, always.
func TestMenuTargetWorksWhenModal(t *testing.T) {
	var imp string
	for _, m := range registeredMethods(t) {
		if m[0] == "menuTargetClass" && m[1] == "worksWhenModal" {
			imp = m[3]
		}
	}
	if imp == "" {
		t.Fatal("the menu target does not implement worksWhenModal")
	}
	b, err := os.ReadFile("app_darwin.go")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`\b` + imp + ` := purego\.NewCallback\(func\(_, _ uintptr\) uintptr \{ return 1 \}\)`).Match(b) {
		t.Errorf("worksWhenModal's implementation %s does not return YES", imp)
	}
}
