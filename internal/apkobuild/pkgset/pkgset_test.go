package pkgset

import (
	"strings"
	"testing"
)

func TestCheckName(t *testing.T) {
	for _, ok := range []string{"a", "bash", "python-3.13", "libstdc++", "a_b", "9p", strings.Repeat("a", 128)} {
		if err := CheckName(ok); err != nil {
			t.Errorf("CheckName(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"", "-a", ".a", "_a", "+a", "A", "a b", "a/b", "a=1", "a\x00", "é", strings.Repeat("a", 129)} {
		if err := CheckName(bad); err == nil {
			t.Errorf("CheckName(%q) passed", bad)
		}
	}
	if err := CheckName("Bad"); err == nil || !strings.Contains(err.Error(), `"Bad"`) {
		t.Errorf("error does not quote the name: %v", err)
	}
}
