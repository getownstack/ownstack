package catalog

import "testing"

// Whole values only, as Python's re.fullmatch reads it.
func TestMatchesPattern(t *testing.T) {
	if !(Param{}).MatchesPattern("anything") {
		t.Error("a param without a pattern must accept every value")
	}
	alt := Param{Pattern: `^a|b$`}
	for v, want := range map[string]bool{"a": true, "b": true, "ab": false, "ax": false, "xb": false} {
		if alt.MatchesPattern(v) != want {
			t.Errorf("%s matches %q = %v, want %v", alt.Pattern, v, !want, want)
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
			Aspects:   []Aspect{{ID: "a", Default: "p"}},
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
