package updater

import "testing"

func TestIsNewer(t *testing.T) {
	cases := []struct {
		latest, current string
		want            bool
	}{
		{"0.3.2", "0.3.1", true},
		{"0.3.1", "0.3.1", false},
		{"0.3.0", "0.3.1", false},
		{"1.0.0", "0.9.9", true},
		{"0.10.0", "0.9.9", true},
		{"0.3.2", "0.3.2-rc1", true},
		{"0.3.2-rc2", "0.3.2-rc1", true},
		{"0.3.2-rc1", "0.3.2", false},
	}
	for _, tc := range cases {
		got := isNewer(tc.latest, tc.current)
		if got != tc.want {
			t.Errorf("isNewer(%q, %q) = %v, want %v", tc.latest, tc.current, got, tc.want)
		}
	}
}

func TestSplitSemver(t *testing.T) {
	got := splitSemver("0.10.2-rc1")
	want := [3]int{0, 10, 2}
	if got != want {
		t.Errorf("splitSemver = %v, want %v", got, want)
	}
}
