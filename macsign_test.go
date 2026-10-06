package caboose

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bfreis/caboose/internal/shelltest"
)

// stubRcodesign is a rcodesign that behaves as 0.29.0's does (read from its
// source): notary-submit logs the ID, notary-wait logs a poll state and
// exits 0 on a verdict, 1 with "Error: ..." otherwise. What each
// notary-wait call does is the next line of $STUB/waits; once they are
// used up the last one repeats. Every call is appended to $STUB/calls.
const stubRcodesign = `#!/bin/sh
echo "$*" >> "$STUB/calls"
case $1 in
sign) exit 0 ;;
notary-submit)
	[ -e "$STUB/noid" ] || echo "created submission ID: 11111111-2222-3333-4444-555555555555" >&2
	exit 0 ;;
notary-log) echo '{"status": "Invalid", "issues": [{"message": "the binary is not signed"}]}'; exit 0 ;;
notary-wait)
	n=$(grep -c '^notary-wait' "$STUB/calls")
	line=$(sed -n "${n}p" "$STUB/waits")
	[ -n "$line" ] || line=$(tail -n 1 "$STUB/waits")
	case $line in
	Accepted | Invalid | Rejected) echo "poll state after 0s: $line" >&2; exit 0 ;;
	progress) echo "poll state after 0s: InProgress" >&2; echo "Error: reached time limit waiting for notarization to complete" >&2; exit 1 ;;
	error) echo "Error: error sending request for url (https://appstoreconnect.apple.com/notary/v2/submissions/x)" >&2; exit 1 ;;
	esac ;;
esac
exit 2
`

func TestMacsignNotarization(t *testing.T) {
	const id = "11111111-2222-3333-4444-555555555555"
	tests := []struct {
		name    string
		waits   string
		noID    bool
		timeout string
		ok      bool
		want    []string // in the output
		waited  int      // notary-wait calls, 0 for any
		logs    int      // notary-log calls
	}{
		{name: "accepted", waits: "Accepted\n", ok: true, want: []string{"notarized (" + id + ")"}, waited: 1},
		{name: "in progress then accepted", waits: "progress\nprogress\nAccepted\n", ok: true, waited: 3},
		{name: "transport errors then accepted", waits: "error\nerror\nAccepted\n", ok: true, want: []string{"retrying in"}, waited: 3},
		{name: "invalid", waits: "Invalid\n", want: []string{"notarization Invalid (" + id + ")", "the binary is not signed"}, waited: 1, logs: 1},
		{name: "rejected", waits: "Rejected\n", want: []string{"notarization Rejected"}, waited: 1, logs: 1},
		{name: "deadline", waits: "progress\n", timeout: "2", want: []string{"no verdict on submission " + id, "xcrun notarytool info " + id, "not resubmitted"}},
		{name: "deadline through errors", waits: "error\n", timeout: "2", want: []string{"no verdict on submission " + id}},
		{name: "no submission id", noID: true, waits: "Accepted\n", want: []string{"gave no submission ID"}},
	}
	for _, sh := range shelltest.Shells(t) {
		for _, tc := range tests {
			t.Run(strings.Join(sh, " ")+"/"+tc.name, func(t *testing.T) {
				dir := t.TempDir()
				stub := filepath.Join(dir, "stub")
				bin := filepath.Join(dir, "bin")
				for _, d := range []string{stub, bin} {
					if err := os.Mkdir(d, 0o755); err != nil {
						t.Fatal(err)
					}
				}
				write := func(p, s string, mode os.FileMode) {
					if err := os.WriteFile(p, []byte(s), mode); err != nil {
						t.Fatal(err)
					}
				}
				write(filepath.Join(bin, "rcodesign"), stubRcodesign, 0o755)
				write(filepath.Join(bin, "zip"), "#!/bin/sh\nexit 0\n", 0o755)
				write(filepath.Join(stub, "waits"), tc.waits, 0o644)
				write(filepath.Join(stub, "calls"), "", 0o644)
				if tc.noID {
					write(filepath.Join(stub, "noid"), "", 0o644)
				}
				for _, f := range []string{"devid.p12", "devid.pass", "api-key.json", "caboose"} {
					write(filepath.Join(dir, f), "x", 0o600)
				}
				timeout := tc.timeout
				if timeout == "" {
					timeout = "600"
				}
				argv := append(append([]string{}, sh[1:]...), "macsign.sh", "caboose", filepath.Join(dir, "caboose"))
				cmd := exec.Command(sh[0], argv...)
				cmd.Env = []string{
					"PATH=" + bin + ":" + os.Getenv("PATH"),
					"STUB=" + stub,
					"TMPDIR=" + dir,
					"MACSIGN_P12=" + filepath.Join(dir, "devid.p12"),
					"MACSIGN_P12_PASSWORD_FILE=" + filepath.Join(dir, "devid.pass"),
					"MACSIGN_API_KEY=" + filepath.Join(dir, "api-key.json"),
					"MACSIGN_NOTARY_TIMEOUT=" + timeout,
					"MACSIGN_NOTARY_RETRY_PAUSE=0",
				}
				out, err := cmd.CombinedOutput()
				if (err == nil) != tc.ok {
					t.Fatalf("ok=%v, err %v:\n%s", tc.ok, err, out)
				}
				for _, w := range tc.want {
					if !strings.Contains(string(out), w) {
						t.Errorf("no %q in:\n%s", w, out)
					}
				}
				calls, _ := os.ReadFile(filepath.Join(stub, "calls"))
				count := func(p string) int { return strings.Count("\n"+string(calls), "\n"+p) }
				if n := count("notary-submit"); n > 1 || (n == 0 && !tc.noID) {
					t.Errorf("notary-submit called %d times:\n%s", n, calls)
				}
				if n := count("notary-wait"); tc.waited > 0 && n != tc.waited {
					t.Errorf("notary-wait called %d times, want %d:\n%s", n, tc.waited, calls)
				}
				if n := count("notary-log"); n != tc.logs {
					t.Errorf("notary-log called %d times, want %d", n, tc.logs)
				}
				if tc.noID && count("notary-wait") != 0 {
					t.Errorf("waited on a submission with no ID")
				}
			})
		}
	}
}
