// Package providers registers every implemented provider.
package providers

import (
	"context"

	"github.com/getownstack/ownstack/internal/catalog"
	"github.com/getownstack/ownstack/internal/providers/cloudflare"
	"github.com/getownstack/ownstack/internal/providers/ghactions"
	"github.com/getownstack/ownstack/internal/providers/github"
	"github.com/getownstack/ownstack/internal/providers/harbor"
	"github.com/getownstack/ownstack/internal/providers/jenkins"
	"github.com/getownstack/ownstack/internal/providers/manualdns"
	"github.com/getownstack/ownstack/internal/providers/ownvps"
	"github.com/getownstack/ownstack/internal/providers/registry"
	"github.com/getownstack/ownstack/internal/providers/slack"
	"github.com/getownstack/ownstack/internal/providers/traefik"
	"github.com/getownstack/ownstack/internal/providers/zot"
	"github.com/getownstack/ownstack/internal/stack"
)

var implementations = map[string]stack.Provider{
	catalog.Key("compute", "own-vps"): ownvps.Provider{},
	catalog.Key("dns", "cloudflare"):  cloudflare.Provider{},
	catalog.Key("dns", "manual"):      manualdns.Provider{},
	catalog.Key("ingress", "traefik"): traefik.Provider{},
	catalog.Key("vcs", "github"):      github.Provider{},
	catalog.Key("images", "harbor"):   harbor.Provider{},
	catalog.Key("images", "zot"):      zot.Provider{},
	catalog.Key("images", "registry"): registry.Provider{},
	catalog.Key("alerts", "slack"):    slack.Provider{},
	catalog.Key("cicd", "jenkins"):    jenkins.Provider{},
	catalog.Key("cicd", "gh-actions"): ghactions.Provider{},
}

// Lookup returns the implementation of a provider.
func Lookup(aspect, id string) (stack.Provider, bool) {
	p, ok := implementations[catalog.Key(aspect, id)]
	return p, ok
}

// Catalog returns the catalog with implemented providers marked available.
func Catalog() *catalog.Catalog {
	implemented := map[string]bool{}
	for key := range implementations {
		implemented[key] = true
	}
	return catalog.Default().WithAvailability(implemented)
}

// RepoBootstrapper is implemented by VCS providers that can create the
// cluster repo, commit the manifest, and check it out on the node.
type RepoBootstrapper interface {
	Bootstrap(ctx context.Context, rt *stack.Runtime, manifestYAML []byte, dir string) error
}
