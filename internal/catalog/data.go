package catalog

// Version of the catalog format exported to the stack designer.
const Version = "1"

// Default returns the Ownstack aspects and presets. The providers are not
// part of it: each provider package declares its own Spec, and
// internal/providers adds them (providers.Catalog).
func Default() *Catalog {
	return &Catalog{
		Version: Version,
		Aspects: []Aspect{
			{
				ID: "compute", Name: "Compute", Tag: "COMPUTE", Required: true, Default: "own-vps",
				Contract: Contract{
					Summary: "A Kubernetes node Ownstack can install onto, reachable on a public IP.",
					Outputs: []Output{
						{Key: "kubeconfig", Description: "Admin kubeconfig for the cluster."},
						{Key: "publicIP", Description: "Address DNS records point at."},
						{Key: "storageClass", Description: "Default storage class for persistent volumes."},
					},
				},
			},
			{
				ID: "dns", Name: "DNS", Tag: "DNS", Required: true, Default: "cloudflare",
				Consumes: []string{"compute"},
				Contract: Contract{
					Summary: "The domain and its wildcard resolve to the node, and ingress can prove domain ownership for TLS.",
					Outputs: []Output{
						{Key: "records", Description: "<domain> and *.<domain> resolve to the compute public IP."},
						{Key: "acmeSolver", Description: "The DNS-01 provider certificates are validated with, and its credentials."},
					},
				},
			},
			{
				ID: "ingress", Name: "Ingress & TLS", Tag: "INGRESS", Required: true, Default: "traefik",
				Consumes: []string{"compute", "dns"},
				Contract: Contract{
					Summary: "Routes HTTPS hostnames to services with automatically issued certificates.",
					Outputs: []Output{
						{Key: "entryPoint", Description: "Entry point name services attach to."},
						{Key: "certResolver", Description: "Certificate resolver name for TLS."},
					},
				},
			},
			{
				ID: "vcs", Name: "Version control", Tag: "VCS", Required: true, Default: "github",
				Consumes: []string{"compute"},
				Contract: Contract{
					Summary: "Hosts the customer-owned cluster repo and application repos, with a credential CI can clone with.",
					Outputs: []Output{
						{Key: "owner", Description: "Account or organization that owns the repos."},
						{Key: "clusterRepo", Description: "Name and clone URL of the cluster repo, which holds ownstack.yaml."},
						{Key: "demoRepo", Description: "Demo application repo, when the stack has one."},
						{Key: "credential", Description: "Username and token for cloning and repository discovery."},
					},
				},
			},
			{
				ID: "images", Name: "Image registry", Tag: "IMAGES", Required: true, Default: "zot",
				Consumes: []string{"compute", "ingress"},
				Contract: Contract{
					Summary: "A private OCI registry with a push credential for CI and a pull secret in every environment.",
					Outputs: []Output{
						{Key: "host", Description: "Registry hostname."},
						{Key: "project", Description: "Repository namespace images are pushed under."},
						{Key: "pushCredential", Description: "Username and password CI pushes with."},
						{Key: "pullSecret", Description: "Name of the image pull secret created in each environment namespace."},
					},
				},
			},
			{
				ID: "cicd", Name: "CI/CD", Tag: "CI/CD", Required: true, Default: "gh-actions",
				Consumes: []string{"compute", "ingress", "vcs", "images"},
				Contract: Contract{
					Summary: "Discovers application repos, builds images, pushes them, and deploys Helm charts to the environments.",
					Outputs: []Output{
						{Key: "url", Description: "CI web UI."},
						{Key: "pipelineConvention", Description: "infrastructure/Jenkinsfile, infrastructure/<app>/Dockerfile and infrastructure/<app>/helm."},
					},
				},
			},
		},
		Presets: []Preset{
			{
				ID: "starter", Name: "Starter", MinMemoryGB: 2, Default: true,
				Summary: "Fits a 2 GB server. Zot registry in your cluster, builds on GitHub Actions, so the server only runs your apps.",
				Components: map[string]string{
					"compute": "own-vps", "dns": "cloudflare", "ingress": "traefik",
					"vcs": "github", "images": "zot", "cicd": "gh-actions",
				},
			},
			{
				ID: "standard", Name: "Standard", MinMemoryGB: 4,
				Summary: "Everything self-hosted for 4 GB+ servers: Harbor registry and Jenkins CI with pod agents.",
				Components: map[string]string{
					"compute": "own-vps", "dns": "cloudflare", "ingress": "traefik",
					"vcs": "github", "images": "harbor", "cicd": "jenkins",
				},
			},
		},
	}
}
