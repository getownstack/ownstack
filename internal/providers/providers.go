// Package providers registers every provider: its catalog entry (Spec) and
// its implementation.
package providers

import (
	"github.com/getownstack/ownstack/internal/catalog"
	"github.com/getownstack/ownstack/internal/providers/cloudflare"
	"github.com/getownstack/ownstack/internal/providers/ghactions"
	"github.com/getownstack/ownstack/internal/providers/github"
	"github.com/getownstack/ownstack/internal/providers/harbor"
	"github.com/getownstack/ownstack/internal/providers/jenkins"
	"github.com/getownstack/ownstack/internal/providers/ownvps"
	"github.com/getownstack/ownstack/internal/providers/traefik"
	"github.com/getownstack/ownstack/internal/providers/zot"
	"github.com/getownstack/ownstack/internal/stack"
)

type provider struct {
	spec catalog.Provider
	impl stack.Provider
}

// registry is every provider, in the order the catalog lists them.
var registry = []provider{
	{ownvps.Spec, ownvps.Provider{}},
	{cloudflare.Spec, cloudflare.Provider{}},
	{traefik.Spec, traefik.Provider{}},
	{github.Spec, github.Provider{}},
	{zot.Spec, zot.Provider{}},
	{harbor.Spec, harbor.Provider{}},
	{ghactions.Spec, ghactions.Provider{}},
	{jenkins.Spec, jenkins.Provider{}},
}

// Lookup returns the implementation of a provider.
func Lookup(aspect, id string) (stack.Provider, bool) {
	for _, p := range registry {
		if p.spec.Aspect == aspect && p.spec.ID == id {
			return p.impl, true
		}
	}
	return nil, false
}

// Catalog returns the catalog: the aspects and presets, and every provider's
// Spec.
func Catalog() *catalog.Catalog {
	cat := catalog.Default()
	for _, p := range registry {
		cat.Providers = append(cat.Providers, p.spec)
	}
	return cat
}
