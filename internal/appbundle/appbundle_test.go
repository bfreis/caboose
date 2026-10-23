package appbundle

import (
	"errors"
	"io/fs"
	"strings"
	"testing"
)

const wantPlist = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>CFBundleDisplayName</key>
	<string>Caboose</string>
	<key>CFBundleExecutable</key>
	<string>caboose</string>
	<key>CFBundleIdentifier</key>
	<string>local.caboose.checkout</string>
	<key>CFBundleName</key>
	<string>Caboose</string>
	<key>CFBundlePackageType</key>
	<string>APPL</string>
	<key>CFBundleShortVersionString</key>
	<string>1.4.2</string>
	<key>CFBundleVersion</key>
	<string>1.4.2</string>
	<key>LSMinimumSystemVersion</key>
	<string>13.0</string>
	<key>LSUIElement</key>
	<true/>
	<key>NSHighResolutionCapable</key>
	<true/>
</dict>
</plist>
`

func TestPlist(t *testing.T) {
	if got := string(Plist(Info{ID: CheckoutID, Version: "v1.4.2"})); got != wantPlist {
		t.Errorf("Plist:\n%s\nwant:\n%s", got, wantPlist)
	}
}

func TestPlistEscapes(t *testing.T) {
	got := string(Plist(Info{ID: `a&b<c>"d`, Version: "v1.0.0-3-gabc&<x>"}))
	if !strings.Contains(got, "<string>a&amp;b&lt;c&gt;&#34;d</string>") {
		t.Errorf("identifier not escaped:\n%s", got)
	}
	if strings.Contains(got, "&<") || strings.Contains(got, "<x>") {
		t.Errorf("raw text in the plist:\n%s", got)
	}
	id, err := ParseIdentifier([]byte(got))
	if err != nil || id != `a&b<c>"d` {
		t.Errorf("ParseIdentifier = %q, %v", id, err)
	}
}

func TestBundleVersion(t *testing.T) {
	for v, want := range map[string]string{
		"v1.2.3":              "1.2.3",
		"1.2.3":               "1.2.3",
		"v0.3.2":              "0.3.2",
		"v1.2.3-4-gabcdef":    "0.0.0",
		"v1.2.3-dirty":        "0.0.0",
		"v1.2.3-rc.1":         "0.0.0",
		"dev":                 "0.0.0",
		"":                    "0.0.0",
		"v1.2.3&amp;":         "0.0.0",
		"v1.2.3\n":            "0.0.0",
		"v1234567890.0.0":     "0.0.0",
		"(devel)":             "0.0.0",
		"v1.2.3 v4.5.6":       "0.0.0",
		"v1.2":                "0.0.0",
		"v1.2.3.4":            "0.0.0",
		"\u202ev1.2.3":        "0.0.0",
		"v1.2.3/../../x":      "0.0.0",
		"v01.2.3":             "01.2.3",
		"v999999999.0.999999": "999999999.0.999999",
	} {
		if got := BundleVersion(v); got != want {
			t.Errorf("BundleVersion(%q) = %q, want %q", v, got, want)
		}
	}
}

func TestParseIdentifier(t *testing.T) {
	good := map[string]string{
		"ours":      wantPlist,
		"first key": `<plist version="1.0"><dict><key>CFBundleIdentifier</key><string>x.y</string></dict></plist>`,
		"nested ignored": `<plist><dict><key>Other</key><dict><key>CFBundleIdentifier</key><string>no</string></dict>` +
			`<key>CFBundleIdentifier</key><string>yes</string></dict></plist>`,
		"array skipped": `<plist><dict><key>A</key><array><string>CFBundleIdentifier</string></array><key>CFBundleIdentifier</key><string>z</string></dict></plist>`,
	}
	want := map[string]string{"ours": CheckoutID, "first key": "x.y", "nested ignored": "yes", "array skipped": "z"}
	for name, p := range good {
		id, err := ParseIdentifier([]byte(p))
		if err != nil || id != want[name] {
			t.Errorf("%s: %q, %v; want %q", name, id, err, want[name])
		}
	}
	huge := strings.Replace(wantPlist, "<dict>", "<dict>"+strings.Repeat(" ", MaxPlist), 1)
	bad := map[string]string{
		"huge":         huge,
		"empty":        "",
		"not xml":      "CFBundleIdentifier = local.caboose.checkout",
		"unclosed":     `<plist><dict><key>CFBundleIdentifier</key><string>x`,
		"mismatched":   `<plist><dict><key>CFBundleIdentifier</key><string>x</key></dict></plist>`,
		"no key":       `<plist><dict><key>CFBundleName</key><string>x</string></dict></plist>`,
		"not a plist":  `<html><dict><key>CFBundleIdentifier</key><string>x</string></dict></html>`,
		"no dict":      `<plist><array><key>CFBundleIdentifier</key><string>x</string></array></plist>`,
		"not a string": `<plist><dict><key>CFBundleIdentifier</key><integer>1</integer></dict></plist>`,
		"twice": `<plist><dict><key>CFBundleIdentifier</key><string>a</string>` +
			`<key>CFBundleIdentifier</key><string>local.caboose.checkout</string></dict></plist>`,
		"empty id": `<plist><dict><key>CFBundleIdentifier</key><string></string></dict></plist>`,
		"entity":   `<!DOCTYPE plist [<!ENTITY e "local.caboose.checkout">]><plist><dict><key>CFBundleIdentifier</key><string>&e;</string></dict></plist>`,
	}
	for name, p := range bad {
		if id, err := ParseIdentifier([]byte(p)); err == nil {
			t.Errorf("%s: accepted, %q", name, id)
		}
	}
}

// plists is a fake filesystem of Info.plist files for IsAppLaunch.
func plists(m map[string]string) func(string) ([]byte, error) {
	return func(p string) ([]byte, error) {
		if s, ok := m[p]; ok {
			return []byte(s), nil
		}
		return nil, fs.ErrNotExist
	}
}

func idPlist(id string) string { return string(Plist(Info{ID: id, Version: "v1.0.0"})) }

func TestIsAppLaunch(t *testing.T) {
	read := plists(map[string]string{
		"/Applications/Caboose.app/Contents/Info.plist": idPlist(CheckoutID),
		"/w/Other.app/Contents/Info.plist":              idPlist("com.example.other"),
		"/w/Bad.app/Contents/Info.plist":                "<plist>",
		"/w/Huge.app/Contents/Info.plist":               idPlist(CheckoutID) + strings.Repeat(" ", MaxPlist),
		"/w/Renamed.app/Contents/Info.plist":            idPlist(CheckoutID),
	})
	exe := "/Applications/Caboose.app/Contents/MacOS/caboose"
	for _, c := range []struct {
		exe  string
		args []string
		want bool
	}{
		{exe, nil, true},
		{exe, []string{}, true},
		{exe, []string{"-psn_0_1"}, true},
		{"/w/Renamed.app/Contents/MacOS/caboose", nil, true},
		{"/Applications/Caboose.app/Contents/MacOS/../MacOS/caboose", nil, true},
		{exe, []string{"-psn_0_1", "x"}, false},
		{exe, []string{"status"}, false},
		{exe, []string{"app"}, false},
		{exe, []string{"-psn"}, false},
		{exe, []string{""}, false},
		{"/usr/local/bin/caboose", nil, false},
		{"/Applications/Caboose.app/Contents/MacOS/caboose-vmm", nil, false},
		{"/Applications/Caboose.app/Contents/Resources/caboose", nil, false},
		{"/Applications/Caboose/Contents/MacOS/caboose", nil, false},
		{"/w/.app/Contents/MacOS/caboose", nil, false},
		{"/w/Other.app/Contents/MacOS/caboose", nil, false},
		{"/w/Bad.app/Contents/MacOS/caboose", nil, false},
		{"/w/Huge.app/Contents/MacOS/caboose", nil, false},
		{"/w/Missing.app/Contents/MacOS/caboose", nil, false},
		{"caboose", nil, false},
		{"", nil, false},
	} {
		if got := IsAppLaunch(c.exe, c.args, read); got != c.want {
			t.Errorf("IsAppLaunch(%q, %q) = %v", c.exe, c.args, got)
		}
	}
}

func TestInCheckoutBundle(t *testing.T) {
	read := plists(map[string]string{
		"/a/Caboose.app/Contents/Info.plist": idPlist(CheckoutID),
		"/b/Caboose.app/Contents/Info.plist": idPlist("com.example.release"),
	})
	for exe, want := range map[string]bool{
		"/a/Caboose.app/Contents/MacOS/caboose": true,
		"/b/Caboose.app/Contents/MacOS/caboose": false,
		"/c/Caboose.app/Contents/MacOS/caboose": false,
		"/a/caboose":                            false,
	} {
		if got := InCheckoutBundle(exe, read); got != want {
			t.Errorf("InCheckoutBundle(%q) = %v", exe, got)
		}
	}
	// Whatever the reader fails with.
	if InCheckoutBundle("/a/Caboose.app/Contents/MacOS/caboose", func(string) ([]byte, error) { return nil, errors.New("no") }) {
		t.Error("a failed read counted")
	}
}

func TestKnown(t *testing.T) {
	if !Known(CheckoutID) || Known("") || Known("local.caboose") || Known(CheckoutID+" ") {
		t.Error("Known")
	}
}

// A Mac's file system and Launch Services take names without case, so a
// bundle is one in any case.
func TestBundleOf(t *testing.T) {
	for exe, want := range map[string]string{
		"/A/Caboose.app/Contents/MacOS/caboose":    "/A/Caboose.app",
		"/A/X.APP/Contents/MacOS/caboose":          "/A/X.APP",
		"/A/x.App/contents/macos/CABOOSE":          "/A/x.App",
		"/A/x.app/Contents/MacOS/../MacOS/Caboose": "/A/x.app",
		"/A/.APP/Contents/MacOS/caboose":           "",
		"/A/x.apps/Contents/MacOS/caboose":         "",
		"/A/x.app/Contents/Resources/caboose":      "",
		"/A/x.app/Contents/MacOS/caboose-vmm":      "",
		"caboose":                                  "",
	} {
		if got := BundleOf(exe); got != want {
			t.Errorf("BundleOf(%q) = %q, want %q", exe, got, want)
		}
	}
}
