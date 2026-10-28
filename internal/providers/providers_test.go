package providers

import (
	"testing"

	"github.com/getownstack/ownstack/internal/catalog"
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
