package catalog

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"
)

// Whole values only.
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
		"secret without env": {Key: "k", Kind: KindSecret},
		"unanchored pattern": {Key: "k", Kind: KindString, Pattern: `a`, PatternHelp: "must be a"},
		"unbalanced pattern": {Key: "k", Kind: KindString, Pattern: `^a)|(b$`, PatternHelp: "must be a"},
		"reserved key":       {Key: "values", Kind: KindString},
	} {
		if err := base(p).Validate(); err == nil {
			t.Errorf("%s: Validate accepted %+v", name, p)
		}
	}
	if err := base(Param{Key: "k", Kind: KindString, Default: "a", Pattern: `^a$`, PatternHelp: "must be a"}).Validate(); err != nil {
		t.Error(err)
	}
}

func TestValidateChecksPresets(t *testing.T) {
	cat := &Catalog{
		Aspects:   []Aspect{{ID: "a", Default: "p"}},
		Providers: []Provider{{ID: "p", Aspect: "a"}, {ID: "q", Aspect: "a"}},
		Presets:   []Preset{{ID: "x", Components: map[string]string{"a": "q"}}},
	}
	if err := cat.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"p", "missing"} {
		cat.Presets[0].Components["a"] = id
		if err := cat.Validate(); err == nil {
			t.Errorf("preset naming %s accepted", id)
		}
	}
}

// The JSON the designer reads: catalog v2.
func TestJSONShape(t *testing.T) {
	data, err := json.Marshal(Default())
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	json.Unmarshal(data, &doc)
	if len(doc) != 4 || doc["memoryFitRatio"] != 0.8 {
		t.Errorf("top level = %v", keys(doc))
	}
	aspect := doc["aspects"].([]any)[0].(map[string]any)
	if strings.Join(keys(aspect), ",") != "default,id,name,summary" {
		t.Errorf("aspect keys = %v", keys(aspect))
	}
	starter := doc["presets"].([]any)[0].(map[string]any)
	if strings.Join(keys(starter), ",") != "components,id,minMemoryGB,name" || len(starter["components"].(map[string]any)) != 0 {
		t.Errorf("starter = %v", starter)
	}
}

func keys(m map[string]any) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestMemoryFits(t *testing.T) {
	for _, tc := range []struct {
		total, need int
		fits        bool
	}{
		{3800, 3928, true}, // a 4 GB server and the Standard preset
		{3143, 3928, true}, // 80% of the estimate, rounded up
		{3142, 3928, false},
		{1642, 1028, true},
		{0, 1, false},
	} {
		if got := MemoryFits(tc.total, tc.need); got != tc.fits {
			t.Errorf("MemoryFits(%d, %d) = %v", tc.total, tc.need, got)
		}
	}
	if got := MinServerMiB(1028); got != 823 {
		t.Errorf("MinServerMiB(1028) = %d", got)
	}
}
