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

func TestLibraryRef(t *testing.T) {
	for version, want := range map[string]string{
		"ownstackctl-v0.3.0":      "ownstackctl-v0.3.0",
		"ownstackctl-v0.4.0-rc.1": "ownstackctl-v0.4.0-rc.1",
		"dev":                     "main",
		"v0.3.0":                  "main",
	} {
		if got := LibraryRef(version); got != want {
			t.Errorf("LibraryRef(%q) = %q, want %q", version, got, want)
		}
	}
	if LibraryRepoURL != "https://github.com/getownstack/ownstack.git" {
		t.Errorf("LibraryRepoURL = %q", LibraryRepoURL)
	}
}
