package providers

import (
	"strings"
	"testing"

	"github.com/getownstack/ownstack/internal/catalog"
	"github.com/getownstack/ownstack/internal/stack"
)

// Every registered provider has a Spec and an implementation, and every
// provider the catalog names (aspect defaults, presets) is registered.
func TestCatalogIsConsistent(t *testing.T) {
	cat := Catalog()
	if err := cat.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, p := range registry {
		if p.spec.ID == "" || p.spec.Aspect == "" || p.spec.Name == "" || p.impl == nil {
			t.Errorf("incomplete registration %+v", p.spec)
		}
	}
	for _, p := range cat.Providers {
		if _, ok := Lookup(p.Aspect, p.ID); !ok {
			t.Errorf("catalog provider %s has no implementation", catalog.Key(p.Aspect, p.ID))
		}
	}
	for _, a := range cat.Aspects {
		if _, ok := Lookup(a.ID, a.Default); !ok {
			t.Errorf("aspect %s defaults to %s, which is not implemented", a.ID, a.Default)
		}
	}
	for _, preset := range cat.Presets {
		for aspect, id := range preset.Components {
			if _, ok := Lookup(aspect, id); !ok {
				t.Errorf("preset %s uses %s/%s, which is not implemented", preset.ID, aspect, id)
			}
		}
	}
	// The Starter preset is the aspects' defaults.
	for _, preset := range cat.Presets {
		if !preset.Default {
			continue
		}
		for _, a := range cat.Aspects {
			if preset.Components[a.ID] != a.Default {
				t.Errorf("default preset %s uses %s for %s, the aspect default is %s", preset.ID, preset.Components[a.ID], a.ID, a.Default)
			}
		}
	}
}

func TestPatterns(t *testing.T) {
	param := func(aspect, id, key string) catalog.Param {
		t.Helper()
		p, ok := Catalog().Provider(aspect, id)
		if !ok {
			t.Fatalf("no provider %s/%s", aspect, id)
		}
		param, ok := p.Param(key)
		if !ok {
			t.Fatalf("no param %s/%s.%s", aspect, id, key)
		}
		return param
	}
	cases := []struct {
		param       catalog.Param
		good, wrong []string
	}{
		{param("images", "zot", "pullSecretName"),
			[]string{"registry-credentials", "harbor-robot", "a", "pull.secret-1"},
			[]string{"", "Harbor", "harbor_robot", "-x", "x-", "a b", "x;rm", strings.Repeat("a", 254)}},
		{param("images", "zot", "project"),
			[]string{"product", "apps", "my-org/team", "a__b", "my--app", "a.b_c"},
			[]string{"", "Apps", "a/", "/a", "a//b", "a_-b", "a___b", "-a", "a b", "a$b"}},
		{param("images", "zot", "username"),
			[]string{"ownstack", "me@example.com", "robot$ci", "José"},
			[]string{"", "a:b", "a b", "a\tb", "a\nb", "a\x7fb"}},
		{param("compute", "own-vps", "k3sVersion"),
			[]string{"v1.33.4+k3s1", "1.33.4+k3s1", "v1.33.4", "1.33.4", "v1.33.10+k3s12", "v1.34.0-rc1+k3s1"},
			[]string{"v1.33", "latest", "v1.33.4+k3s", "v1.33.4-+k3s1", "v1.33.4+k3s1;x", "v1.33.4-rc1;x", "v1.33.4 "}},
		{param("cicd", "jenkins", "extraPlugins"),
			[]string{"git:5.11.0", "htmlpublisher:429 build-timeout:1.41", "kubernetes:4560.vc25b_83890c34  git:5.12.0", "a.b_c-d:1.0-SNAPSHOT+b"},
			[]string{"", "git", "git:", ":1", "Git:1", "-git:1", "git:1 ", " git:1", "git:1,slack:2", "git:1;rm", "git:1\nslack:2", "git:1\tslack:2", "git:$(x)", "git:1:2"}},
		{param("images", "zot", "keepTags"),
			[]string{"0", "10"},
			[]string{"-1", "x"}},
	}
	for _, tc := range cases {
		for _, v := range tc.good {
			if !tc.param.MatchesPattern(v) {
				t.Errorf("%s: %q should match %s", tc.param.Key, v, tc.param.Pattern)
			}
		}
		for _, v := range tc.wrong {
			if tc.param.MatchesPattern(v) {
				t.Errorf("%s: %q should not match %s", tc.param.Key, v, tc.param.Pattern)
			}
		}
	}
}

// Both CI providers have the libraryRef setting, and its help names the
// repository the libraries come from.
func TestLibraryRefNamesTheLibraryRepo(t *testing.T) {
	for _, id := range []string{"jenkins", "gh-actions"} {
		p, _ := Catalog().Provider("cicd", id)
		param, ok := p.Param("libraryRef")
		if !ok || param.Default != "" || !strings.Contains(param.Help, stack.LibraryRepo+" ") {
			t.Errorf("%s libraryRef = %+v", id, param)
		}
	}
}
