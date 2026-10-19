package stack

import "testing"

func TestChartVersion(t *testing.T) {
	for _, tc := range []struct{ chart, want, got string }{
		{"harbor-1.15.0", "1.15.0", "1.15.0"},
		{"harbor-1.15.0", "1.16.0", "1.15.0"},
		{"zot-0.1.60-rc.1", "0.1.60", "0.1.60-rc.1"},
		{"zot-0.1.60-rc.1", "0.1.60-rc.1", "0.1.60-rc.1"},
		{"my-app-2.0.0+build.5", "", "2.0.0+build.5"},
		{"chart-v2-1.0.0", "", "1.0.0"},
	} {
		if got := chartVersion(tc.chart, tc.want); got != tc.got {
			t.Errorf("chartVersion(%q, %q) = %q, want %q", tc.chart, tc.want, got, tc.got)
		}
	}
}
