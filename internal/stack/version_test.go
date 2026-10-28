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
	for _, c := range []struct{ setting, version, want string }{
		{"main", "ownstackctl-v0.3.0", "main"},
		{"my-branch", "dev", "my-branch"},
		{"", "ownstackctl-v0.3.0", "ownstackctl-v0.3.0"},
		{"", "ownstackctl-v0.3.0-rc1", "ownstackctl-v0.3.0-rc1"},
		{"", "dev", "main"},
		{"", "v0.3.0", "main"},
	} {
		if got := LibraryRef(c.setting, c.version); got != c.want {
			t.Errorf("LibraryRef(%q, %q) = %q, want %q", c.setting, c.version, got, c.want)
		}
	}
	if LibraryRepoURL != "https://github.com/getownstack/ownstack.git" {
		t.Errorf("LibraryRepoURL = %q", LibraryRepoURL)
	}
}
