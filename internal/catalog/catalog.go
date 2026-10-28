// Package catalog describes what an Ownstack stack can be made of.
//
// A stack has six aspects (compute, dns, ingress, vcs, images, cicd). Each
// aspect is a contract that its providers implement; the Go types of the
// contracts are in internal/stack. The catalog is plain data, exported as
// JSON (`ownstackctl catalog`) for the Ownstack stack designer.
package catalog

import (
	"fmt"
	"math"
	"regexp"
	"slices"
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

// MemoryFitRatio is how much of a stack's memory estimate a server must
// have: the estimates are generous, so 80% is enough.
const MemoryFitRatio = 0.8

// MinServerMiB is the least memory a server needs for a stack whose
// providers' estimates add up to need MiB.
func MinServerMiB(need int) int {
	return int(math.Ceil(float64(need) * MemoryFitRatio))
}

// MemoryFits reports whether a stack that needs about need MiB fits a server
// with total MiB.
func MemoryFits(total, need int) bool {
	return total >= MinServerMiB(need)
}

type Aspect struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Summary string `json:"summary"`
	// Default is the provider a manifest gets when it names none: together
	// the defaults are the Starter preset.
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
	Default     string `json:"default,omitempty"`
	Placeholder string `json:"placeholder,omitempty"`
	// Pattern is a regular expression, anchored with ^ and $, that a set
	// value must match. PatternHelp completes "<label> ..." when a value
	// does not match.
	Pattern     string `json:"pattern,omitempty"`
	PatternHelp string `json:"patternHelp,omitempty"`
	Advanced    bool   `json:"advanced,omitempty"`
	// Env is the environment variable a secret is read from.
	Env string `json:"env,omitempty"`
	// Generate marks secrets ownstackctl may generate and keep on the node
	// when the user does not supply one (for example initial admin passwords).
	Generate bool `json:"generate,omitempty"`
}

func (p Param) IsSecret() bool { return p.Kind == KindSecret }

// MatchesPattern reports whether the whole value matches the param's
// Pattern. A param without a pattern accepts every value.
func (p Param) MatchesPattern(value string) bool {
	if p.Pattern == "" {
		return true
	}
	re, err := compilePattern(p.Pattern)
	return err == nil && re.MatchString(value)
}

var compiledPatterns sync.Map // pattern -> *regexp.Regexp

// compilePattern compiles pattern to match whole values, so a top-level
// alternation such as ^a|b$ cannot match a prefix or a suffix.
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

// Label names one of p's params in messages, such as "Own VPS public IP".
func (p Provider) Label(param Param) string {
	label := param.Label
	// Lowercase a leading capital unless it starts an acronym.
	if len(label) > 1 && label[0] >= 'A' && label[0] <= 'Z' && label[1] >= 'a' && label[1] <= 'z' {
		label = string(label[0]+('a'-'A')) + label[1:]
	}
	return p.Name + " " + label
}

// Preset is a recommended combination of providers for a kind of server.
type Preset struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	MinMemoryGB int    `json:"minMemoryGB"`
	// Components are the providers that differ from the aspect defaults.
	Components map[string]string `json:"components"`
}

type Catalog struct {
	Aspects        []Aspect   `json:"aspects"`
	Providers      []Provider `json:"providers"`
	Presets        []Preset   `json:"presets"`
	MemoryFitRatio float64    `json:"memoryFitRatio"`
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

// ProviderIDs lists an aspect's providers.
func (c *Catalog) ProviderIDs(aspect string) []string {
	var ids []string
	for _, p := range c.Providers {
		if p.Aspect == aspect {
			ids = append(ids, p.ID)
		}
	}
	return ids
}

// Validate checks the catalog's internal consistency. It runs in tests.
func (c *Catalog) Validate() error {
	seen := map[string]bool{}
	for _, a := range c.Aspects {
		if seen[a.ID] {
			return fmt.Errorf("duplicate aspect %q", a.ID)
		}
		seen[a.ID] = true
		if _, ok := c.Provider(a.ID, a.Default); !ok {
			return fmt.Errorf("aspect %q has unknown default provider %q", a.ID, a.Default)
		}
	}
	seen = map[string]bool{}
	for _, p := range c.Providers {
		key := p.Aspect + "/" + p.ID
		if seen[key] {
			return fmt.Errorf("duplicate provider %q", key)
		}
		seen[key] = true
		if _, ok := c.Aspect(p.Aspect); !ok {
			return fmt.Errorf("provider %q references unknown aspect", key)
		}
		params := map[string]bool{"provider": true, "values": true}
		for _, param := range p.Params {
			if params[param.Key] {
				return fmt.Errorf("provider %q has duplicate or reserved param %q", key, param.Key)
			}
			params[param.Key] = true
			if err := checkParam(param); err != nil {
				return fmt.Errorf("param %q of provider %q: %w", param.Key, key, err)
			}
		}
	}
	for _, preset := range c.Presets {
		for aspect, id := range preset.Components {
			a, _ := c.Aspect(aspect)
			if _, ok := c.Provider(aspect, id); !ok {
				return fmt.Errorf("preset %q uses unknown provider %s/%s", preset.ID, aspect, id)
			}
			if id == a.Default {
				return fmt.Errorf("preset %q names %s's default provider %s; list only providers that differ", preset.ID, aspect, id)
			}
		}
	}
	return nil
}

func checkParam(param Param) error {
	switch {
	case !slices.Contains(kinds, param.Kind):
		return fmt.Errorf("unknown kind %q", param.Kind)
	case param.IsSecret() && param.Env == "":
		return fmt.Errorf("secrets need an environment variable")
	case param.IsSecret() && (param.Default != "" || param.Pattern != ""):
		return fmt.Errorf("secrets have no default or pattern")
	case param.Pattern == "" && param.PatternHelp != "":
		return fmt.Errorf("patternHelp without a pattern")
	case param.Pattern == "":
		return nil
	case param.PatternHelp == "":
		return fmt.Errorf("pattern needs patternHelp")
	case !strings.HasPrefix(param.Pattern, "^") || !strings.HasSuffix(param.Pattern, "$") || strings.HasSuffix(param.Pattern, `\$`):
		return fmt.Errorf("pattern %q must be anchored with ^ and $", param.Pattern)
	}
	// Compile it alone too: wrapping can balance a pattern such as ^a)|(b$.
	if _, err := regexp.Compile(param.Pattern); err != nil {
		return err
	}
	if _, err := compilePattern(param.Pattern); err != nil {
		return err
	}
	if param.Default != "" && !param.MatchesPattern(param.Default) {
		return fmt.Errorf("default %q does not match its pattern", param.Default)
	}
	return nil
}
