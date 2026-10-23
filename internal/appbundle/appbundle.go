// Package appbundle is the menu-bar app's macOS bundle, Caboose.app, as
// plain files: its Info.plist, assembling the bundle around the launcher's
// binary, and telling from an executable's path whether it runs from one.
// It is pure Go and works the same on any system: nothing here runs
// AppKit, codesign or the app.
package appbundle

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// The bundle's identifiers. A checkout's bundle (make app) claims no
// domain or account. ReleaseID is a release's, empty until releases ship
// the app: changing an identifier later loses the notification
// authorization and the login item given to the old one.
const (
	CheckoutID = "local.caboose.checkout"
	ReleaseID  = ""
)

// Name is the bundle's directory name; Executable the binary's name in
// Contents/MacOS.
const (
	Name       = "Caboose.app"
	Executable = "caboose"
)

// CheckoutDir is the directory under CABOOSE_HOME a checkout's bundle is
// assembled in (make app), beside the checkout's daemon directory and,
// like it, in no sandbox's mount: never in the checkout, which a sandbox
// may write, since the host runs the bundle, at every login once it is a
// login item.
const CheckoutDir = "app-dev"

// CheckoutPath is where make app puts a checkout's bundle: the one fixed
// place, CABOOSE_HOME/app-dev/Caboose.app.
func CheckoutPath(cabooseHome string) string {
	return filepath.Join(cabooseHome, CheckoutDir, Name)
}

// MaxPlist bounds what ReadPlist reads and ParseIdentifier accepts.
const MaxPlist = 64 << 10

// Known reports whether id is one of caboose's bundle identifiers.
func Known(id string) bool {
	return id == CheckoutID || (ReleaseID != "" && id == ReleaseID)
}

// Info is what the Info.plist says of the bundle.
type Info struct {
	ID      string // the bundle identifier: CheckoutID, or a release's
	Version string // caboose's version, as version.Get gives it
}

// releaseVersion is a release tag's numbers.
var releaseVersion = regexp.MustCompile(`^v?([0-9]{1,9}\.[0-9]{1,9}\.[0-9]{1,9})$`)

// BundleVersion is the version the plist gives: a release's without its
// v, else (a checkout's build, a prerelease) 0.0.0, since macOS reads the
// field as numbers only.
func BundleVersion(v string) string {
	if m := releaseVersion.FindStringSubmatch(v); m != nil {
		return m[1]
	}
	return "0.0.0"
}

// Plist is the bundle's Info.plist, an XML property list. LSUIElement
// keeps the app out of the Dock; there are no entitlements.
func Plist(in Info) []byte {
	v := BundleVersion(in.Version)
	var b bytes.Buffer
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
`)
	str := func(k, v string) {
		fmt.Fprintf(&b, "\t<key>%s</key>\n\t<string>%s</string>\n", escape(k), escape(v))
	}
	yes := func(k string) { fmt.Fprintf(&b, "\t<key>%s</key>\n\t<true/>\n", escape(k)) }
	str("CFBundleDisplayName", "Caboose")
	str("CFBundleExecutable", Executable)
	str("CFBundleIdentifier", in.ID)
	str("CFBundleName", "Caboose")
	str("CFBundlePackageType", "APPL")
	str("CFBundleShortVersionString", v)
	str("CFBundleVersion", v)
	str("LSMinimumSystemVersion", "13.0")
	yes("LSUIElement")
	yes("NSHighResolutionCapable")
	b.WriteString("</dict>\n</plist>\n")
	return b.Bytes()
}

func escape(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// ReadPlist reads the file at path, at most MaxPlist+1 bytes of it, so
// that ParseIdentifier can refuse a larger one.
func ReadPlist(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, MaxPlist+1))
}

// ParseIdentifier is the CFBundleIdentifier of an XML Info.plist: the
// string after that key in the top-level dict. A plist larger than
// MaxPlist, malformed, or without the key is an error.
func ParseIdentifier(plist []byte) (string, error) {
	if len(plist) > MaxPlist {
		return "", errors.New("Info.plist is too large")
	}
	d := xml.NewDecoder(bytes.NewReader(plist))
	d.Strict = true
	// depth: 1 inside <plist>, 2 inside its <dict>.
	depth, key, found := 0, "", ""
	var inKey, inValue bool
	var text strings.Builder
	for {
		tok, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("Info.plist: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			switch {
			case depth == 1 && t.Name.Local != "plist":
				return "", errors.New("Info.plist: not a property list")
			case depth == 2 && t.Name.Local != "dict":
				return "", errors.New("Info.plist: no top-level dict")
			case depth == 3 && t.Name.Local == "key":
				inKey = true
				text.Reset()
			case depth == 3:
				inValue = key == "CFBundleIdentifier" && t.Name.Local == "string"
				text.Reset()
			}
		case xml.CharData:
			if (inKey || inValue) && text.Len()+len(t) <= MaxPlist {
				text.Write(t)
			}
		case xml.EndElement:
			if depth == 3 {
				switch {
				case inKey:
					key, inKey = text.String(), false
				case inValue:
					if found != "" {
						return "", errors.New("Info.plist: CFBundleIdentifier given twice")
					}
					found, inValue, key = text.String(), false, ""
				default:
					key = ""
				}
			}
			depth--
		}
	}
	if found == "" {
		return "", errors.New("Info.plist: no CFBundleIdentifier")
	}
	return found, nil
}

// bundleOf is the bundle directory exe sits in as its executable
// (B.app/Contents/MacOS/caboose), or "". Names compare without case, as
// a Mac's file system and Launch Services do: X.APP/contents/MacOS/caboose
// is the same bundle.
func bundleOf(exe string) string {
	exe = filepath.Clean(exe)
	macos := filepath.Dir(exe)
	contents := filepath.Dir(macos)
	app := filepath.Dir(contents)
	name := filepath.Base(app)
	if !strings.EqualFold(filepath.Base(exe), Executable) || !strings.EqualFold(filepath.Base(macos), "MacOS") ||
		!strings.EqualFold(filepath.Base(contents), "Contents") ||
		len(name) <= len(".app") || !strings.EqualFold(name[len(name)-len(".app"):], ".app") {
		return ""
	}
	return app
}

// BundleOf is the bundle directory exe sits in as its executable
// (B.app/Contents/MacOS/caboose), or "" when it is none.
func BundleOf(exe string) string { return bundleOf(exe) }

// bundleID is the identifier of the bundle exe runs from, or "".
func bundleID(exe string, readPlist func(string) ([]byte, error)) string {
	app := bundleOf(exe)
	if app == "" {
		return ""
	}
	b, err := readPlist(filepath.Join(app, "Contents", "Info.plist"))
	if err != nil {
		return ""
	}
	id, err := ParseIdentifier(b)
	if err != nil {
		return ""
	}
	return id
}

// IsAppLaunch reports whether a launch of exe with args (os.Args[1:]) is
// the menu-bar app's from Finder, open or the login item: exe is the
// executable of a bundle with one of caboose's identifiers, and args are
// none, or a lone -psn_ argument older Launch Services add.
func IsAppLaunch(exe string, args []string, readPlist func(string) ([]byte, error)) bool {
	switch {
	case len(args) == 0:
	case len(args) == 1 && strings.HasPrefix(args[0], "-psn_"):
	default:
		return false
	}
	return Known(bundleID(exe, readPlist))
}

// InCheckoutBundle reports whether exe is the executable of a bundle with
// the checkout's identifier, whatever its version stamp: a make app
// bundle, which uses a checkout's daemon directory.
func InCheckoutBundle(exe string, readPlist func(string) ([]byte, error)) bool {
	return bundleID(exe, readPlist) == CheckoutID
}
