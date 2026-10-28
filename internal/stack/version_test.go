package stack

import (
	"strings"
	"testing"

	"github.com/getownstack/ownstack/internal/catalog"
)

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

// Both CI providers have the libraryRef setting, and its help names the
// repository the libraries come from.
func TestCatalogLibraryRefNamesTheLibraryRepo(t *testing.T) {
	for _, id := range []string{"jenkins", "gh-actions"} {
		p, ok := catalog.Default().Provider("cicd", id)
		if !ok {
			t.Fatalf("no provider %s", id)
		}
		var found bool
		for _, param := range p.Params {
			if param.Key == "libraryRef" {
				found = true
				if param.Default != "" || !strings.Contains(param.Help, LibraryRepo+" ") {
					t.Errorf("%s libraryRef = %+v", id, param)
				}
			}
		}
		if !found {
			t.Errorf("%s has no libraryRef setting", id)
		}
	}
}
