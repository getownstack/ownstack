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
	"sort"
)

// Param kinds.
const (
	KindString   = "string"
	KindHostname = "hostname"
	KindIP       = "ip"
	KindEmail    = "email"
	KindInt      = "int"
	KindBool     = "bool"
	KindEnum     = "enum"
	KindSecret   = "secret"
)

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
}

type Param struct {
	Key         string   `json:"key"`
	Label       string   `json:"label"`
	Help        string   `json:"help,omitempty"`
	Kind        string   `json:"kind"`
	Required    bool     `json:"required,omitempty"`
	Default     string   `json:"default,omitempty"` // may reference {domain} and {admin}
	Placeholder string   `json:"placeholder,omitempty"`
	Enum        []string `json:"enum,omitempty"`
	Advanced    bool     `json:"advanced,omitempty"`
	// Env is the conventional environment variable a secret is read from
	// when the manifest does not reference one explicitly.
	Env string `json:"env,omitempty"`
	// Generate marks secrets ownstackctl may generate and keep on the node
	// when the user does not supply one (for example initial admin passwords).
	Generate bool `json:"generate,omitempty"`
}

func (p Param) IsSecret() bool { return p.Kind == KindSecret }

type Path struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Recommended bool   `json:"recommended,omitempty"`
}

type Resources struct {
	MemoryMiB int `json:"memoryMiB,omitempty"`
	DiskGiB   int `json:"diskGiB,omitempty"`
}

type Provider struct {
	ID          string    `json:"id"`
	Aspect      string    `json:"aspect"`
	Name        string    `json:"name"`
	Mono        string    `json:"mono"`
	Hosting     string    `json:"hosting"`
	Summary     string    `json:"summary"`
	Params      []Param   `json:"params,omitempty"`
	Requires    []string  `json:"requires,omitempty"`
	Paths       []Path    `json:"paths,omitempty"`
	Resources   Resources `json:"resources,omitempty"`
	Available   bool      `json:"available"`
	Recommended bool      `json:"recommended,omitempty"`
}

func (p Provider) Param(key string) (Param, bool) {
	for _, param := range p.Params {
		if param.Key == key {
			return param, true
		}
	}
	return Param{}, false
}

type Catalog struct {
	Version   string     `json:"version"`
	Aspects   []Aspect   `json:"aspects"`
	Providers []Provider `json:"providers"`
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

// WithAvailability returns a copy of the catalog where providers present in
// implemented are marked available.
func (c *Catalog) WithAvailability(implemented map[string]bool) *Catalog {
	out := &Catalog{Version: c.Version, Aspects: c.Aspects}
	out.Providers = make([]Provider, len(c.Providers))
	for i, p := range c.Providers {
		p.Available = implemented[Key(p.Aspect, p.ID)]
		out.Providers[i] = p
	}
	return out
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
		aspect, ok := c.Aspect(p.Aspect)
		if !ok {
			return fmt.Errorf("provider %q references unknown aspect", key)
		}
		for _, req := range p.Requires {
			if !contains(aspect.Consumes, req) {
				return fmt.Errorf("provider %q requires %q which aspect %q does not consume", key, req, p.Aspect)
			}
		}
		params := map[string]bool{}
		for _, param := range p.Params {
			if params[param.Key] {
				return fmt.Errorf("provider %q has duplicate param %q", key, param.Key)
			}
			params[param.Key] = true
			if param.IsSecret() && param.Env == "" {
				return fmt.Errorf("secret %q of provider %q needs a conventional env var", param.Key, key)
			}
		}
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
