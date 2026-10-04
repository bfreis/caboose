package hostlink

import (
	"errors"
	"os/exec"
	"runtime"
	"strings"
)

// System is this machine's Actions: on a Mac, open and osascript; on Linux,
// xdg-open, notify-send, and zenity or kdialog for the dialog.
type System struct{}

// The AppleScripts take what they show as arguments (argv), never spliced
// into the script's text: what the sandbox wrote stays data.
const (
	notifyScript  = `on run argv` + "\n" + `display notification (item 2 of argv) with title (item 1 of argv)` + "\n" + `end run`
	confirmScript = `on run argv` + "\n" +
		`display dialog (item 1 of argv) with title "caboose" buttons {"Don't open", "Open"} default button "Don't open" cancel button "Don't open" with icon caution` + "\n" +
		`return button returned of result` + "\n" + `end run`
)

// OpenURL opens u in the default browser.
func (System) OpenURL(u string) error {
	switch runtime.GOOS {
	case "darwin":
		// u is http(s) (CheckURL), which open never takes for a file.
		return exec.Command("open", u).Run()
	default:
		return run("xdg-open", u)
	}
}

// Notify shows a notification.
func (System) Notify(title, text string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("osascript", "-e", notifyScript, title, text).Run()
	default:
		return run("notify-send", "--", title, text)
	}
}

// Confirm asks with a dialog. Declining is (false, nil); no way to ask is
// an error.
func (System) Confirm(prompt string) (bool, error) {
	switch runtime.GOOS {
	case "darwin":
		out, err := exec.Command("osascript", "-e", confirmScript, prompt).Output()
		if err != nil {
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				return false, nil // the cancel button, or the dialog dismissed
			}
			return false, err
		}
		return strings.TrimSpace(string(out)) == "Open", nil
	default:
		for _, tool := range [][]string{
			{"zenity", "--question", "--title=caboose", "--text", prompt},
			{"kdialog", "--title", "caboose", "--yesno", prompt},
		} {
			if _, err := exec.LookPath(tool[0]); err != nil {
				continue
			}
			err := exec.Command(tool[0], tool[1:]...).Run()
			var ee *exec.ExitError
			switch {
			case err == nil:
				return true, nil
			case errors.As(err, &ee):
				return false, nil
			default:
				return false, err
			}
		}
		return false, errors.New(`no dialog tool (zenity, kdialog) to ask with; set open_urls = "allow" or "off"`)
	}
}

func run(name string, args ...string) error {
	if _, err := exec.LookPath(name); err != nil {
		return errors.New(name + " is not installed on the host")
	}
	return exec.Command(name, args...).Run()
}
