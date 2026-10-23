package uiport

import "testing"

func TestPorts(t *testing.T) {
	for p, want := range map[int]bool{7171: true, 7172: true, 7170: false, 7173: false, 0: false, 80: false} {
		if Is(p) != want {
			t.Errorf("Is(%d) = %v", p, !want)
		}
	}
	if a := Addr(false); a.String() != "127.0.0.1:7171" || !a.Addr().Is4() || !a.Addr().IsLoopback() {
		t.Errorf("Addr(false) = %v", a)
	}
	if a := Addr(true); a.String() != "127.0.0.1:7172" {
		t.Errorf("Addr(true) = %v", a)
	}
	if o := Origin(false); o != "http://127.0.0.1:7171" {
		t.Errorf("Origin(false) = %q", o)
	}
	if o := Origin(true); o != "http://127.0.0.1:7172" {
		t.Errorf("Origin(true) = %q", o)
	}
	for _, c := range []struct {
		lo, hi, want int
		ok           bool
	}{
		{7000, 7999, 7171, true}, {7172, 7172, 7172, true}, {7172, 8000, 7172, true},
		{7171, 7171, 7171, true}, {1, 7170, 0, false}, {7173, 65535, 0, false},
	} {
		if p, ok := Covering(c.lo, c.hi); p != c.want || ok != c.ok {
			t.Errorf("Covering(%d, %d) = %d, %v", c.lo, c.hi, p, ok)
		}
	}
}
