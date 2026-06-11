package agent

import "testing"

func TestCLIVersionAtLeast(t *testing.T) {
	for _, tc := range []struct {
		name string
		cli  string
		min  string
		want bool
	}{
		// Sentinel "no enforcement" cases.
		{"empty min always passes", "0.0.11", "", true},
		{"0.0.0 min always passes", "0.0.11", "0.0.0", true},
		{"v0.0.0 min always passes", "0.0.11", "v0.0.0", true},
		{"empty min passes even for empty cli", "", "", true},

		// Regular comparison.
		{"equal versions pass", "0.0.11", "0.0.11", true},
		{"higher patch passes", "0.0.12", "0.0.11", true},
		{"higher minor passes", "0.1.0", "0.0.99", true},
		{"higher major passes", "1.0.0", "0.99.99", true},
		{"lower patch fails", "0.0.10", "0.0.11", false},
		{"lower minor fails", "0.0.99", "0.1.0", false},
		{"lower major fails", "0.99.99", "1.0.0", false},

		// Normalization: both sides may or may not have `v`.
		{"cli has v, min doesn't", "v0.0.11", "0.0.11", true},
		{"min has v, cli doesn't", "0.0.11", "v0.0.11", true},
		{"both have v", "v0.0.11", "v0.0.11", true},

		// Fail-closed cases.
		{"empty cli + nonzero min fails", "", "0.0.11", false},
		{"unparseable cli fails", "garbage", "0.0.11", false},
		{"unparseable min fails", "0.0.11", "garbage", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := cliVersionAtLeast(tc.cli, tc.min)
			if got != tc.want {
				t.Errorf("cliVersionAtLeast(%q, %q) = %v, want %v", tc.cli, tc.min, got, tc.want)
			}
		})
	}
}
