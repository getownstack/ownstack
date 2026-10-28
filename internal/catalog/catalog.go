// Package catalog describes what an Ownstack stack can be made of.
//
// A stack is a set of aspects (compute, dns, ingress, ...). Each aspect is a
// contract: it names the outputs every implementation must deliver so that
// the aspects consuming it never need to know which provider is behind it.
// Providers are implementations of one aspect. The catalog is plain data so it
// can be exported as JSON for the Ownstack stack designer.
package catalog

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// Param kinds.
const (
	KindString   = "string"
	KindHostname = "hostname"
	KindIP       = "ip"
	KindEmail    = "email"
	KindInt      = "int"
	KindBool     = "bool"
	KindSecret   = "secret"
)

var kinds = []string{KindString, KindHostname, KindIP, KindEmail, KindInt, KindBool, KindSecret}

// Hosting values.
const (
	SelfHosted = "self"
	External   = "external"
)

type Output struct {
	Key         string `json:"key"`
	Description string `json:"description"`
}

type Contract struct {
	Summary string   `json:"summary"`
	Outputs []Output `json:"outputs"`
}

type Aspect struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Tag      string   `json:"tag"`
	Required bool     `json:"required"`
	Contract Contract `json:"contract"`
	// Consumes lists aspects whose contracts this aspect's providers may use.
	Consumes []string `json:"consumes,omitempty"`
	// Default is the provider the Starter preset uses for this aspect.
	Default string `json:"default"`
}

type Param struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Help  string `json:"help,omitempty"`
	// HelpURL points at the page where the user creates this value, such as
	// a token creation form with the right scopes preselected.
	HelpURL     string `json:"helpURL,omitempty"`
	Kind        string `json:"kind"`
	Required    bool   `json:"required,omitempty"`
	Default     string `json:"default,omitempty"` // may reference {domain} and {adminDomain}
	Placeholder string `json:"placeholder,omitempty"`
	// Pattern is a regular expression, anchored with ^ and $, that a set
	// value must match. It uses only syntax Go's regexp and Python's re read
	// the same way, because the Ownstack designer checks it too. PatternHelp
	// completes "<label> ..." when a value does not match.
	Pattern     string `json:"pattern,omitempty"`
	PatternHelp string `json:"patternHelp,omitempty"`
	Advanced    bool   `json:"advanced,omitempty"`
	// Env is the conventional environment variable a secret is read from
	// when the manifest does not reference one explicitly.
	Env string `json:"env,omitempty"`
	// Generate marks secrets ownstackctl may generate and keep on the node
	// when the user does not supply one (for example initial admin passwords).
	Generate bool `json:"generate,omitempty"`
}

func (p Param) IsSecret() bool { return p.Kind == KindSecret }

// MatchesPattern reports whether value matches the param's Pattern. A param
// without a pattern accepts every value.
func (p Param) MatchesPattern(value string) bool {
	if p.Pattern == "" {
		return true
	}
	re, err := compilePattern(p.Pattern)
	return err == nil && re.MatchString(value)
}

var compiledPatterns sync.Map // pattern -> *regexp.Regexp

// compilePattern compiles pattern to match whole values, as Python's
// re.fullmatch does, so a top-level alternation such as ^a|b$ cannot match
// a prefix or suffix in Go only.
func compilePattern(pattern string) (*regexp.Regexp, error) {
	if re, ok := compiledPatterns.Load(pattern); ok {
		return re.(*regexp.Regexp), nil
	}
	re, err := regexp.Compile(`^(?:` + pattern + `)$`)
	if err != nil {
		return nil, err
	}
	compiledPatterns.Store(pattern, re)
	return re, nil
}

// unsharedSyntax finds regular expression syntax that Go and Python read
// differently or only one of them supports: the \d \w \s \b classes (Unicode
// in Python), \A \z \Z anchors, \p classes, \Q quoting, [[:class:]],
// (?flags) or named groups, and {,n} (a repeat in Python, literal in Go).
// Only (?: groups are shared.
var unsharedSyntax = regexp.MustCompile(`\\[dDwWsSbBAzZpPQE]|\[\[:|\(\?[^:]|\{,`)

func checkPattern(pattern string) error {
	if !strings.HasPrefix(pattern, "^") || !strings.HasSuffix(pattern, "$") || strings.HasSuffix(pattern, `\$`) {
		return fmt.Errorf("pattern %q must be anchored with ^ and $", pattern)
	}
	if bad := unsharedSyntax.FindString(pattern); bad != "" {
		return fmt.Errorf("pattern %q uses %q, which Go and Python read differently", pattern, bad)
	}
	// Compile it alone too: wrapping can balance a pattern such as ^a)|(b$.
	if _, err := regexp.Compile(pattern); err != nil {
		return err
	}
	_, err := compilePattern(pattern)
	return err
}

type Resources struct {
	MemoryMiB int `json:"memoryMiB,omitempty"`
	DiskGiB   int `json:"diskGiB,omitempty"`
}

// Provider is one implementation of an aspect. Each provider package
// declares its own as Spec; internal/providers lists them.
type Provider struct {
	ID        string    `json:"id"`
	Aspect    string    `json:"aspect"`
	Name      string    `json:"name"`
	Mono      string    `json:"mono"`
	Hosting   string    `json:"hosting"`
	Summary   string    `json:"summary"`
	Params    []Param   `json:"params,omitempty"`
	Resources Resources `json:"resources,omitempty"`
}

func (p Provider) Param(key string) (Param, bool) {
	for _, param := range p.Params {
		if param.Key == key {
			return param, true
		}
	}
	return Param{}, false
}

// Preset is a recommended combination of providers for a kind of server.
type Preset struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Summary     string            `json:"summary"`
	MinMemoryGB int               `json:"minMemoryGB"`
	Components  map[string]string `json:"components"`
	Default     bool              `json:"default,omitempty"`
}

type Catalog struct {
	Version   string     `json:"version"`
	Aspects   []Aspect   `json:"aspects"`
	Providers []Provider `json:"providers"`
	Presets   []Preset   `json:"presets"`
}

func (c *Catalog) Aspect(id string) (Aspect, bool) {
	for _, a := range c.Aspects {
		if a.ID == id {
			return a, true
		}
	}
	return Aspect{}, false
}

func (c *Catalog) Provider(aspect, id string) (Provider, bool) {
	for _, p := range c.Providers {
		if p.Aspect == aspect && p.ID == id {
			return p, true
		}
	}
	return Provider{}, false
}

func (c *Catalog) ProvidersFor(aspect string) []Provider {
	var out []Provider
	for _, p := range c.Providers {
		if p.Aspect == aspect {
			out = append(out, p)
		}
	}
	return out
}

// Order is the apply order. Every aspect only consumes aspects before it.
func (c *Catalog) Order() []string {
	ids := make([]string, len(c.Aspects))
	for i, a := range c.Aspects {
		ids[i] = a.ID
	}
	return ids
}

func Key(aspect, provider string) string { return aspect + "/" + provider }

// Validate checks the catalog's internal consistency. It runs in tests.
func (c *Catalog) Validate() error {
	aspectIndex := map[string]int{}
	for i, a := range c.Aspects {
		if _, dup := aspectIndex[a.ID]; dup {
			return fmt.Errorf("duplicate aspect %q", a.ID)
		}
		aspectIndex[a.ID] = i
		for _, consumed := range a.Consumes {
			j, ok := aspectIndex[consumed]
			if !ok || j >= i {
				return fmt.Errorf("aspect %q consumes %q which is not ordered before it", a.ID, consumed)
			}
		}
	}
	seen := map[string]bool{}
	for _, p := range c.Providers {
		key := Key(p.Aspect, p.ID)
		if seen[key] {
			return fmt.Errorf("duplicate provider %q", key)
		}
		seen[key] = true
		if _, ok := c.Aspect(p.Aspect); !ok {
			return fmt.Errorf("provider %q references unknown aspect", key)
		}
		params := map[string]bool{}
		for _, param := range p.Params {
			if params[param.Key] {
				return fmt.Errorf("provider %q has duplicate param %q", key, param.Key)
			}
			params[param.Key] = true
			if !contains(kinds, param.Kind) {
				return fmt.Errorf("param %q of provider %q has unknown kind %q", param.Key, key, param.Kind)
			}
			if param.IsSecret() && param.Env == "" {
				return fmt.Errorf("secret %q of provider %q needs a conventional env var", param.Key, key)
			}
			if err := checkParamPattern(param); err != nil {
				return fmt.Errorf("param %q of provider %q: %w", param.Key, key, err)
			}
		}
	}
	for _, a := range c.Aspects {
		if _, ok := c.Provider(a.ID, a.Default); !ok {
			return fmt.Errorf("aspect %q has unknown default provider %q", a.ID, a.Default)
		}
	}
	for _, preset := range c.Presets {
		for aspect, id := range preset.Components {
			if _, ok := c.Provider(aspect, id); !ok {
				return fmt.Errorf("preset %q uses unknown provider %s/%s", preset.ID, aspect, id)
			}
		}
		for _, a := range c.Aspects {
			if _, ok := preset.Components[a.ID]; a.Required && !ok {
				return fmt.Errorf("preset %q is missing required aspect %q", preset.ID, a.ID)
			}
		}
	}
	return nil
}

func checkParamPattern(param Param) error {
	if param.Pattern == "" {
		if param.PatternHelp != "" {
			return fmt.Errorf("patternHelp without a pattern")
		}
		return nil
	}
	if param.IsSecret() {
		return fmt.Errorf("secrets cannot have a pattern")
	}
	if param.PatternHelp == "" {
		return fmt.Errorf("pattern needs patternHelp")
	}
	if err := checkPattern(param.Pattern); err != nil {
		return err
	}
	if param.Default != "" && !strings.Contains(param.Default, "{") && !param.MatchesPattern(param.Default) {
		return fmt.Errorf("default %q does not match its pattern", param.Default)
	}
	return nil
}

// SecretEnvNames lists every conventional secret environment variable, sorted.
func (c *Catalog) SecretEnvNames() []string {
	var names []string
	for _, p := range c.Providers {
		for _, param := range p.Params {
			if param.IsSecret() {
				names = append(names, param.Env)
			}
		}
	}
	sort.Strings(names)
	return names
}

func contains(list []string, v string) bool {
	for _, item := range list {
		if item == v {
			return true
		}
	}
	return false
}
