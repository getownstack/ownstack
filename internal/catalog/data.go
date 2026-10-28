package catalog

// Default returns the Ownstack aspects, in apply order, and the presets. The
// providers are not part of it: each provider package declares its own Spec,
// and internal/providers adds them (providers.Catalog).
func Default() *Catalog {
	return &Catalog{
		Aspects: []Aspect{
			{ID: "compute", Name: "Compute", Default: "own-vps",
				Summary: "A Kubernetes node Ownstack can install onto, reachable on a public IP."},
			{ID: "dns", Name: "DNS", Default: "cloudflare",
				Summary: "The domain and its wildcard resolve to the node, and ingress can prove domain ownership for TLS."},
			{ID: "ingress", Name: "Ingress & TLS", Default: "traefik",
				Summary: "Routes HTTPS hostnames to services with automatically issued certificates."},
			{ID: "vcs", Name: "Version control", Default: "github",
				Summary: "Hosts the application repos and a private cluster repo that mirrors ownstack.yaml, with a credential CI can clone with."},
			{ID: "images", Name: "Image registry", Default: "zot",
				Summary: "A private OCI registry with a push credential for CI and a pull secret in every environment."},
			{ID: "cicd", Name: "CI/CD", Default: "gh-actions",
				Summary: "Builds the apps of the listed repos, pushes their images, and deploys their Helm charts to the environments."},
		},
		Presets: []Preset{
			{ID: "starter", Name: "Starter", MinMemoryGB: 2, Components: map[string]string{}},
			{ID: "standard", Name: "Standard", MinMemoryGB: 4, Components: map[string]string{"images": "harbor", "cicd": "jenkins"}},
		},
		MemoryFitRatio: MemoryFitRatio,
	}
}
