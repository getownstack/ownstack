package pluginlock

import (
	"fmt"
	"sort"
	"strings"
)

// Result is a resolved plugin set for one Jenkins core.
type Result struct {
	// Plugins is every plugin to install, sorted by name: the requested ones
	// and their transitive non-optional dependencies, each at the update
	// center's version.
	Plugins []Entry
	// RequiredBy names, for each plugin, the plugins that depend on it (none
	// for a requested plugin).
	RequiredBy map[string][]string
	// Problems make the set uninstallable: a plugin missing from the update
	// center, one that needs a newer core, or a dependency older than a
	// plugin requires.
	Problems []string
	// Advisories are published security warnings that match a chosen
	// version, or the core.
	Advisories []Advisory
	// Deprecated lists chosen plugins the update center marks deprecated.
	Deprecated []string
}

// Advisory is a security warning that applies to a locked version.
type Advisory struct {
	Plugin, Version, ID, Message, URL string
}

func (a Advisory) String() string {
	return fmt.Sprintf("%s %s: %s %s (%s)", a.Plugin, a.Version, a.ID, a.Message, a.URL)
}

// Resolve computes the plugin set for core from the requested plugins: each
// plugin and each non-optional dependency, transitively, at the version uc
// offers. It never picks another version; it reports what does not fit.
func Resolve(uc *UpdateCenter, core string, requested []string) *Result {
	res := &Result{RequiredBy: map[string][]string{}}
	chosen := map[string]Plugin{}
	queue := append([]string(nil), requested...)
	seen := map[string]bool{}
	for _, name := range requested {
		seen[name] = true
	}
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		p, ok := uc.Plugins[name]
		if !ok {
			if by := res.RequiredBy[name]; len(by) > 0 {
				res.Problems = append(res.Problems, fmt.Sprintf("%s (needed by %s) is not in the update center for Jenkins %s", name, strings.Join(by, ", "), core))
			} else {
				res.Problems = append(res.Problems, fmt.Sprintf("%s is not in the update center for Jenkins %s", name, core))
			}
			continue
		}
		chosen[name] = p
		for _, d := range p.Dependencies {
			if d.Optional {
				continue
			}
			res.RequiredBy[d.Name] = append(res.RequiredBy[d.Name], name)
			if !seen[d.Name] {
				seen[d.Name] = true
				queue = append(queue, d.Name)
			}
		}
	}

	names := make([]string, 0, len(chosen))
	for name := range chosen {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		p := chosen[name]
		res.Plugins = append(res.Plugins, Entry{Name: name, Version: p.Version})
		sort.Strings(res.RequiredBy[name])
		if p.RequiredCore != "" && Compare(p.RequiredCore, core) > 0 {
			res.Problems = append(res.Problems, fmt.Sprintf("%s %s requires Jenkins %s, newer than %s", name, p.Version, p.RequiredCore, core))
		}
		// Jenkins refuses to load a plugin whose dependency, optional ones
		// included when installed, is older than it requires.
		for _, d := range p.Dependencies {
			dep, ok := chosen[d.Name]
			if ok && Compare(dep.Version, d.Version) < 0 {
				res.Problems = append(res.Problems, fmt.Sprintf("%s %s requires %s %s or newer, but the update center offers %s", name, p.Version, d.Name, d.Version, dep.Version))
			}
		}
		if _, ok := uc.Deprecations[name]; ok {
			res.Deprecated = append(res.Deprecated, name)
		}
	}
	for _, w := range uc.Warnings {
		switch w.Type {
		case "core":
			if w.Affects("core", core) {
				res.Advisories = append(res.Advisories, Advisory{"core", core, w.ID, w.Message, w.URL})
			}
		case "plugin":
			if p, ok := chosen[w.Name]; ok && w.Affects(w.Name, p.Version) {
				res.Advisories = append(res.Advisories, Advisory{w.Name, p.Version, w.ID, w.Message, w.URL})
			}
		}
	}
	sort.Slice(res.Advisories, func(i, j int) bool {
		a, b := res.Advisories[i], res.Advisories[j]
		if a.Plugin != b.Plugin {
			return a.Plugin < b.Plugin
		}
		return a.ID < b.ID
	})
	return res
}
