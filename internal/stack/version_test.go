package stack

import "testing"

func TestReleaseTag(t *testing.T) {
	for version, want := range map[string]string{
		"ownstackctl-v0.2.1":     "ownstackctl-v0.2.1",
		"ownstackctl-v1.0.0-rc1": "ownstackctl-v1.0.0-rc1",
		"dev":                    "",
		"":                       "",
		"v0.2.1":                 "",
		"ownstackctl-v0.2":       "",
		"ownstackctl-v0.2.1 x":   "",
	} {
		if got := ReleaseTag(version); got != want {
			t.Errorf("ReleaseTag(%q) = %q, want %q", version, got, want)
		}
	}
}
