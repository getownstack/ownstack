package pluginlock

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

// Expected results come from Jenkins itself (hudson.util.VersionNumber in
// version-number 1.14, bundled with Jenkins 2.580.1).
func TestCompareMatchesJenkins(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
	}{
		{"1.0", "1", 0},
		{"1.0.0", "1", 0},
		{"1-1", "1.1", -1},
		{"1-sp", "1", 1},
		{"1-rc", "1", -1},
		{"1-ga", "1", 0},
		{"1.0-SNAPSHOT", "1.0", -1},
		{"1.0-alpha-1", "1.0-beta-1", -1},
		{"1.0-beta-1", "1.0-rc1", -1},
		{"1.0a1", "1.0-alpha-1", 1},
		{"1.0b2", "1.0-beta-2", 1},
		{"1.0-cr1", "1.0-rc1", 0},
		{"1.0-rc1", "1.0", -1},
		{"2.263.v941d21defef7", "2.263", 1},
		{"2.263.v941d21defef7", "2.263.1", -1},
		{"4560.vc25b_83890c34", "4557.ve746270f672f", 1},
		{"3.2.3-194.v31a_b_f7a_b_5a_81", "3.2.3-195.v1", -1},
		{"3.2.3-194.v31a_b_f7a_b_5a_81", "3.2.3", 1},
		{"2.30.1.83-289.v8426fcd19371", "2.30.1.80-261.v00c0e2618ec3", 1},
		{"1.10", "1.9", 1},
		{"007", "7", 0},
		{"1.010", "1.10", 0},
		{"2.516.1", "2.516.3", -1},
		{"2.580.1", "2.516.3", 1},
		{"2.580", "2.580.1", -1},
		{"2.581", "2.580.1", 1},
		{"425", "1.36", 1},
		{"1.36", "1.32", 1},
		{"1.0-xyz", "1.0-abc", 1},
		{"1.0-xyz", "1.0", 1},
		{"1.x", "1.1", -1},
		{"1.0.v", "1.0", 1},
		{"1-a", "1-a1", 1},
		{"1.0-1", "1.0.1", -1},
		{"1.2-3-4", "1.2-3", 1},
		{"1..2", "1.0.2", 0},
		{"12345678901234567890", "12345678901234567889", 1},
		{"1.0 beta", "1.0", 1},
	} {
		if got := Compare(c.a, c.b); got != c.want {
			t.Errorf("Compare(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
		if got := Compare(c.b, c.a); got != -c.want {
			t.Errorf("Compare(%q, %q) = %d, want %d", c.b, c.a, got, -c.want)
		}
	}
}

func fixture(t *testing.T) *UpdateCenter {
	t.Helper()
	data, err := os.ReadFile("testdata/update-center.json")
	if err != nil {
		t.Fatal(err)
	}
	uc, err := ParseUpdateCenter(data)
	if err != nil {
		t.Fatal(err)
	}
	return uc
}

func TestParseUpdateCenterJSONP(t *testing.T) {
	data, _ := os.ReadFile("testdata/update-center.json")
	uc, err := ParseUpdateCenter([]byte("updateCenter.post(\n" + string(data) + "\n);\n"))
	if err != nil || uc.Core.Version != "2.580.1" || uc.Plugins["lib"].Name != "lib" {
		t.Fatalf("JSONP: %v %+v", err, uc)
	}
	if _, err := ParseUpdateCenter([]byte(`{"core":{"version":""}}`)); err == nil {
		t.Error("empty update center accepted")
	}
}

func TestResolveTransitive(t *testing.T) {
	res := Resolve(fixture(t), "2.580.1", []string{"app"})
	if len(res.Problems) != 0 || len(res.Advisories) != 0 {
		t.Fatalf("problems %v, advisories %v", res.Problems, res.Advisories)
	}
	// extra is optional and not requested, so it stays out.
	want := []Entry{{"api", "2.263.v941d21defef7"}, {"app", "4560.vc25b_83890c34"}, {"lib", "1.10"}}
	if !reflect.DeepEqual(res.Plugins, want) {
		t.Errorf("plugins = %v", res.Plugins)
	}
	if got := res.RequiredBy["api"]; !reflect.DeepEqual(got, []string{"app", "lib"}) {
		t.Errorf("api required by %v", got)
	}

	// Cycles terminate, and requesting a dependency again changes nothing.
	res = Resolve(fixture(t), "2.580.1", []string{"cycle-a", "lib", "api"})
	if len(res.Problems) != 0 || len(res.Plugins) != 4 {
		t.Errorf("cycle: %v %v", res.Plugins, res.Problems)
	}
}

func TestResolveProblems(t *testing.T) {
	uc := fixture(t)
	for _, c := range []struct {
		core      string
		requested []string
		want      string
	}{
		{"2.580.1", []string{"newer-core"}, "newer-core 3.0 requires Jenkins 2.581, newer than 2.580.1"},
		{"2.516.1", []string{"app"}, "app 4560.vc25b_83890c34 requires Jenkins 2.516.3, newer than 2.516.1"},
		{"2.580.1", []string{"needs-newer-lib"}, "needs-newer-lib 1.0 requires lib 1.11 or newer, but the update center offers 1.10"},
		// An installed optional dependency must be new enough too.
		{"2.580.1", []string{"needs-newer-extra", "extra"}, "needs-newer-extra 1.0 requires extra 2.0 or newer, but the update center offers 1.0"},
		{"2.580.1", []string{"dangling"}, "ghost (needed by dangling) is not in the update center for Jenkins 2.580.1"},
		{"2.580.1", []string{"nope"}, "nope is not in the update center for Jenkins 2.580.1"},
	} {
		res := Resolve(uc, c.core, c.requested)
		if !strings.Contains(strings.Join(res.Problems, "\n"), c.want) {
			t.Errorf("%v on %s: problems %q, want %q", c.requested, c.core, res.Problems, c.want)
		}
	}
	if res := Resolve(uc, "2.580.1", []string{"needs-newer-extra"}); len(res.Problems) != 0 {
		t.Errorf("an optional dependency that is not installed is no problem: %v", res.Problems)
	}
}

func TestResolveAdvisories(t *testing.T) {
	uc := fixture(t)
	res := Resolve(uc, "2.580.1", []string{"vulnerable", "java-pattern", "app"})
	var ids []string
	for _, a := range res.Advisories {
		ids = append(ids, a.Plugin+" "+a.Version+" "+a.ID)
	}
	// SECURITY-2 covers app versions up to 4000.v1 only; java-pattern's
	// possessive pattern falls back to its 1.1..1.5 range.
	if want := []string{"java-pattern 1.2 SECURITY-3", "vulnerable 1.36 SECURITY-1"}; !reflect.DeepEqual(ids, want) {
		t.Errorf("advisories = %v, want %v", ids, want)
	}
	if !strings.Contains(res.Advisories[1].String(), "Stored XSS vulnerability (https://www.jenkins.io/security/advisory/2026-01-01/#SECURITY-1)") {
		t.Errorf("advisory = %s", res.Advisories[1])
	}
	res = Resolve(uc, "2.516.1", []string{"lib"})
	if len(res.Advisories) != 1 || res.Advisories[0].Plugin != "core" || res.Advisories[0].ID != "core-2_516_2" {
		t.Errorf("core advisories = %v", res.Advisories)
	}
	if res := Resolve(uc, "2.580.1", []string{"retired"}); !reflect.DeepEqual(res.Deprecated, []string{"retired"}) {
		t.Errorf("deprecated = %v", res.Deprecated)
	}
}

func TestWarningFallbackRange(t *testing.T) {
	v := WarningVersion{FirstVersion: "1.1", LastVersion: "1.5", Pattern: "1[.][0-9]++"}
	for version, want := range map[string]bool{"1.0": false, "1.1": true, "1.5": true, "1.5.1": false, "2.0": false} {
		if v.Matches(version) != want {
			t.Errorf("%s matches = %v", version, !want)
		}
	}
	// A top-level alternation still matches whole versions only.
	alt := WarningVersion{Pattern: "(1[.]3[01])(|[.-].+)|(1[.]32)"}
	for version, want := range map[string]bool{"1.30": true, "1.31.2": true, "1.32": true, "1.320": false, "11.32": false, "1.33": false} {
		if alt.Matches(version) != want {
			t.Errorf("%s matches %s = %v", version, alt.Pattern, !want)
		}
	}
}

func TestLockRoundTrip(t *testing.T) {
	l := New("5.9.72", "2.580.1", []Entry{{"api", "2.263.v941d21defef7"}, {"app", "4560.vc25b_83890c34"}})
	data := l.Format()
	want := Header + "\n# chart: 5.9.72\n# jenkins: 2.580.1\n# image: jenkins/jenkins:2.580.1-jdk21\napi:2.263.v941d21defef7\napp:4560.vc25b_83890c34\n"
	if string(data) != want {
		t.Fatalf("format:\n%s", data)
	}
	parsed, err := ParseLock(data)
	if err != nil || !reflect.DeepEqual(parsed, l) || parsed.ImageTag() != "2.580.1-jdk21" {
		t.Fatalf("parse: %v %+v", err, parsed)
	}
	head := Header + "\n# chart: 5.9.72\n# jenkins: 2.580.1\n# image: jenkins/jenkins:2.580.1-jdk21\n"
	for name, bad := range map[string]string{
		"no header":   "# chart: 5.9.72\n# jenkins: 2.580.1\n# image: jenkins/jenkins:2.580.1-jdk21\na:1\n",
		"unsorted":    head + "b:1\na:1\n",
		"repeated":    head + "a:1\na:2\n",
		"no version":  head + "a\n",
		"blank line":  head + "a:1\n\nb:1\n",
		"empty":       head,
		"wrong image": strings.Replace(head, "jdk21", "jdk25", 1) + "a:1\n",
		"bad chart":   strings.Replace(head, "5.9.72", "latest", 1) + "a:1\n",
	} {
		if _, err := ParseLock([]byte(bad)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestParseInput(t *testing.T) {
	names, err := ParseInput([]byte("# top-level plugins\nkubernetes\n\n  git   # trailing comment\nslack\n"))
	if err != nil || !reflect.DeepEqual(names, []string{"kubernetes", "git", "slack"}) {
		t.Errorf("names = %v, %v", names, err)
	}
	for _, bad := range []string{"git\ngit\n", "git:5.11.0\n", "# nothing\n", "a b\n"} {
		if _, err := ParseInput([]byte(bad)); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestExtraPlugins(t *testing.T) {
	extra, err := ParseExtra("  htmlpublisher:427  git:5.12.0 htmlpublisher:428 ")
	if err != nil || !reflect.DeepEqual(extra, []Entry{{"htmlpublisher", "428"}, {"git", "5.12.0"}}) {
		t.Fatalf("extra = %v, %v", extra, err)
	}
	if _, err := ParseExtra("git"); err == nil {
		t.Error("a plugin without a version accepted")
	}
	l := New("5.9.72", "2.580.1", []Entry{{"git", "5.11.0"}, {"slack", "1"}})
	got := l.With(extra)
	if want := []Entry{{"git", "5.12.0"}, {"htmlpublisher", "428"}, {"slack", "1"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("With = %v", got)
	}
}

func TestChanges(t *testing.T) {
	prev := New("5.8.79", "2.516.1", []Entry{{"a", "1.0"}, {"b", "2.0"}, {"c", "4557.ve746270f672f"}, {"gone", "1"}})
	next := New("5.9.72", "2.580.1", []Entry{{"a", "1.0"}, {"b", "1.9"}, {"c", "4560.vc25b_83890c34"}, {"new", "3"}})
	want := []string{
		"chart 5.8.79 → 5.9.72", "Jenkins 2.516.1 → 2.580.1",
		"downgraded b 2.0 → 1.9", "upgraded c 4557.ve746270f672f → 4560.vc25b_83890c34", "added new 3", "removed gone 1",
	}
	if got := Changes(prev, next); !reflect.DeepEqual(got, want) {
		t.Errorf("changes = %q", got)
	}
	if got := Changes(next, next); len(got) != 0 {
		t.Errorf("no changes = %q", got)
	}
	if got := Changes(nil, next); got[0] != "chart (none) → 5.9.72" || len(got) != 6 {
		t.Errorf("first lock = %q", got)
	}
}
