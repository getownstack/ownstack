package catalog

import (
	"strings"
	"testing"
)

func TestDefaultIsValid(t *testing.T) {
	if err := Default().Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestPatterns(t *testing.T) {
	provider := func(aspect, id string) Provider {
		t.Helper()
		p, ok := Default().Provider(aspect, id)
		if !ok {
			t.Fatalf("no provider %s/%s", aspect, id)
		}
		return p
	}
	param := func(aspect, id, key string) Param {
		t.Helper()
		p, ok := provider(aspect, id).Param(key)
		if !ok {
			t.Fatalf("no param %s/%s.%s", aspect, id, key)
		}
		return p
	}
	cases := []struct {
		param       Param
		good, wrong []string
	}{
		{param("images", "zot", "pullSecretName"),
			[]string{"registry-credentials", "harbor-robot", "a", "pull.secret-1"},
			[]string{"", "Harbor", "harbor_robot", "-x", "x-", "a b", "x;rm", strings.Repeat("a", 254)}},
		{param("images", "registry", "project"),
			[]string{"product", "apps", "my-org/team", "a__b", "my--app", "a.b_c"},
			[]string{"", "Apps", "a/", "/a", "a//b", "a_-b", "a___b", "-a", "a b", "a$b"}},
		{param("images", "zot", "username"),
			[]string{"ownstack", "me@example.com", "robot$ci", "José"},
			[]string{"", "a:b", "a b", "a\tb", "a\nb", "a\x7fb"}},
		{param("compute", "own-vps", "k3sVersion"),
			[]string{"v1.33.4+k3s1", "1.33.4+k3s1", "v1.33.4", "1.33.4", "v1.33.10+k3s12", "v1.34.0-rc1+k3s1"},
			[]string{"v1.33", "latest", "v1.33.4+k3s", "v1.33.4-+k3s1", "v1.33.4+k3s1;x", "v1.33.4-rc1;x", "v1.33.4 "}},
		{param("cicd", "jenkins", "deployBranch"),
			[]string{"main", "release/1.2", "Feature_x-1"},
			[]string{"", "-x", "/main", "a b", "main;rm", "a$b", "a&b", "a<b"}},
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
	if !(Param{}).MatchesPattern("anything") {
		t.Error("a param without a pattern must accept every value")
	}
	// Whole values only, as Python's re.fullmatch reads it.
	alt := Param{Pattern: `^a|b$`}
	for v, want := range map[string]bool{"a": true, "b": true, "ab": false, "ax": false, "xb": false} {
		if alt.MatchesPattern(v) != want {
			t.Errorf("%s matches %q = %v, want %v", alt.Pattern, v, !want, want)
		}
	}
}

func TestPullSecretNameIsSharedAndRequired(t *testing.T) {
	for id, def := range map[string]string{"harbor": "harbor-robot", "zot": "registry-credentials", "registry": "registry-credentials"} {
		p, _ := Default().Provider("images", id)
		param, ok := p.Param("pullSecretName")
		if !ok || !param.Required || param.Default != def || param.Pattern == "" {
			t.Errorf("%s pullSecretName = %+v", id, param)
		}
	}
}

func TestCheckPatternRejectsUnsharedSyntax(t *testing.T) {
	for _, pattern := range []string{
		`[a-z]+`, `^[a-z]+`, `[a-z]+$`, `^a\$`,
		`^\d+$`, `^\w+$`, `^[^\s]+$`, `^a\b$`, `^\p{L}+$`, `^[[:alpha:]]+$`, `^(?i)a$`, `^(?P<x>a)$`, `^\Aa$`, `^a\z$`, `^a{,2}$`,
		`^(a$`, `^a)|(b$`,
	} {
		if err := checkPattern(pattern); err == nil {
			t.Errorf("checkPattern(%q) accepted it", pattern)
		}
	}
	for _, pattern := range []string{`^(?:a|b)+$`, `^[^:\x00-\x20]+$`, `^v?[0-9]+\.[0-9]+$`} {
		if err := checkPattern(pattern); err != nil {
			t.Errorf("checkPattern(%q) = %v", pattern, err)
		}
	}
}

func TestValidateChecksParams(t *testing.T) {
	base := func(p Param) *Catalog {
		return &Catalog{
			Aspects:   []Aspect{{ID: "a"}},
			Providers: []Provider{{ID: "p", Aspect: "a", Params: []Param{p}}},
		}
	}
	for name, p := range map[string]Param{
		"unknown kind":       {Key: "k", Kind: "colour"},
		"pattern no help":    {Key: "k", Kind: KindString, Pattern: `^a$`},
		"help no pattern":    {Key: "k", Kind: KindString, PatternHelp: "must be a"},
		"default mismatch":   {Key: "k", Kind: KindString, Default: "b", Pattern: `^a$`, PatternHelp: "must be a"},
		"pattern on secret":  {Key: "k", Kind: KindSecret, Env: "K", Pattern: `^a$`, PatternHelp: "must be a"},
		"unanchored pattern": {Key: "k", Kind: KindString, Pattern: `a`, PatternHelp: "must be a"},
	} {
		if err := base(p).Validate(); err == nil {
			t.Errorf("%s: Validate accepted %+v", name, p)
		}
	}
	ok := Param{Key: "k", Kind: KindString, Default: "{domain}", Pattern: `^a$`, PatternHelp: "must be a"}
	if err := base(ok).Validate(); err != nil {
		t.Errorf("templated defaults are not pattern-checked: %v", err)
	}
}
