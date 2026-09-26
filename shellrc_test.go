package caboose

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// shellrc.bash reads shell.d's *.sh and *.bash in one byte order, X.sh
// before X.bash, skipping everything else; what the files declare stays
// declared (the loader runs at the top level, not in a function), the
// locale is as it was, and the loader leaves no names of its own behind.
func TestShellrc(t *testing.T) {
	needBash(t)
	home := t.TempDir()
	dir := filepath.Join(home, ".config/caboose/shell.d")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"20-bar.bash": "", "10-foo.sh": "", "20-bar.sh": "", "10-foo.bash": "",
		"05-only.bash": "", "10-foo.c.sh": "", "B.sh": "", "a.sh": "",
		"30-z.zsh": "", ".hidden.sh": "", "README": "", "10-foo.sh~": "",
		"40-decl.sh":  "declare -a arr=(x y)\nalias ll='ls -l'\n",
		"50-fails.sh": "false\n",
	} {
		body = "order+=(" + name + ")\n" + body
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	script := `shopt -s expand_aliases
order=()
. ./shellrc.bash
echo "${order[*]}"
echo "arr=${arr[*]} lc_all=${LC_ALL-unset}"
alias ll
compgen -v __caboose_ || echo "no leftovers"
`
	cmd := exec.Command("bash", "-c", script)
	// Without LC_ALL, which the loader sets and has to unset again.
	cmd.Env = []string{"HOME=" + home, "LANG=C.UTF-8", "PATH=" + os.Getenv("PATH")}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	want := "05-only.bash 10-foo.sh 10-foo.bash 10-foo.c.sh 20-bar.sh 20-bar.bash 40-decl.sh 50-fails.sh B.sh a.sh\n" +
		"arr=x y lc_all=unset\nalias ll='ls -l'\nno leftovers\n"
	if string(out) != want {
		t.Errorf("got:\n%s\nwant:\n%s", out, want)
	}
}

// With no shell.d, sourcing it does nothing and succeeds.
func TestShellrcNone(t *testing.T) {
	needBash(t)
	cmd := exec.Command("bash", "-c", ". ./shellrc.bash && echo ok")
	cmd.Env = append(os.Environ(), "HOME="+t.TempDir())
	if out, err := cmd.CombinedOutput(); err != nil || string(out) != "ok\n" {
		t.Errorf("%v: %q", err, out)
	}
}
