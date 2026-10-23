//go:build darwin

package checkpoint

import (
	"path/filepath"
	"testing"
)

// On a Mac the store and the target share an APFS volume, so a restore
// clones the whole tree: the copy still shares no inode with the store,
// even where checkpoints link files between them, and it goes through the
// same rename and making safe.
func TestRestoreWholeCloneDarwin(t *testing.T) {
	f := newRfix(t)
	for _, n := range []string{"a", "b", "sub/c"} {
		f.write(t, n, n)
	}
	mustWrite(t, filepath.Join(f.top, "proj", ".git", "config"), gitConfigWithCommand, 0o644)
	f.take(t)
	r := f.take(t)
	if r.Meta.Linked == 0 {
		t.Fatal("the second checkpoint linked nothing")
	}
	o := f.opts(t)
	o.Copy = Options{}
	to := filepath.Join(f.parent, "x")
	res, err := f.restore(t, r.ID.String(), to, o)
	if err != nil {
		t.Fatal(err)
	}
	if res.Method != MethodClonefile {
		t.Skipf("restored by %s, not a whole clone", res.Method)
	}
	store := inodes(t, f.s.Dir())
	for ino, p := range inodes(t, to) {
		if sp, ok := store[ino]; ok {
			t.Errorf("%s shares its inode with %s", p, sp)
		}
	}
	if readT(t, filepath.Join(to, ".git", "config")) != gitConfigWithCommand || readT(t, filepath.Join(to, "sub", "c")) != "sub/c" {
		t.Error("contents")
	}
}
