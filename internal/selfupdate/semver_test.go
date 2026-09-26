package selfupdate

import "testing"

func TestValid(t *testing.T) {
	for v, want := range map[string]bool{
		"v1.2.3": true, "v0.0.1": true, "v1.0.0-rc.1": true, "v1.2.3-4-gabcdef-dirty": true,
		"1.2.3": false, "v1.2": false, "dev": false, "v01.2.3": false, "": false, "v1.2.3+meta": false,
	} {
		if got := Valid(v); got != want {
			t.Errorf("Valid(%q) = %v", v, got)
		}
	}
}

func TestCompare(t *testing.T) {
	// Each is less than the next.
	order := []string{"v0.9.9", "v1.0.0-alpha", "v1.0.0-alpha.1", "v1.0.0-alpha.beta", "v1.0.0-beta.2",
		"v1.0.0-beta.11", "v1.0.0-rc.1", "v1.0.0", "v1.0.1", "v1.2.0", "v1.10.0", "v2.0.0"}
	for i := range order {
		for j := range order {
			want := sign(i - j)
			if got := Compare(order[i], order[j]); got != want {
				t.Errorf("Compare(%s, %s) = %d, want %d", order[i], order[j], got, want)
			}
		}
	}
}
