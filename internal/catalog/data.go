package catalog

// Version of the catalog format exported to the stack designer.
const Version = "1"

// JenkinsChartVersion is the jenkins/jenkins chart the Jenkins provider's
// plugin lock (internal/providers/jenkins/plugins.lock) is resolved and
// verified for. lockgen rewrites this line; a test keeps the two equal.
const JenkinsChartVersion = "5.10.0"

// Jenkins plugin pins: space-separated <name>:<version> pairs.
const jenkinsPluginsPattern = `^[a-z0-9][a-z0-9_.-]*:[A-Za-z0-9_.+-]+( +[a-z0-9][a-z0-9_.-]*:[A-Za-z0-9_.+-]+)*$`

// Value formats shared by several params, in the regular expression syntax Go
// and Python share (see Param.Pattern).
const (
	// A Kubernetes object name (DNS-1123 subdomain), such as a pull secret.
	kubeNamePattern = `^[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?$`
	kubeNameHelp    = "must be a lowercase Kubernetes name: letters, digits, '-' and '.'"
	// A repository path as OCI registries accept it: lowercase components
	// joined by '.', '_', '__', dashes, or '/'.
	imagePathPattern = `^[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*(?:/[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*)*$`
	imagePathHelp    = "must be lowercase letters and digits joined by '.', '_', '-' or '/' (registries reject uppercase)"
)

// Branch settings of the cicd providers. The default branch (the deployBranch
// setting when a provider has one, else DefaultBranch) always deploys into the
// environment setting, the production environment. The branches setting maps
// further branches to environments; the manifest validates and parses it.
const (
	ParamEnvironment  = "environment"
	ParamDeployBranch = "deployBranch"
	ParamBranches     = "branches"
	DefaultBranch     = "main"
)

// branchesParam is the cicd providers' setting for extra deploy branches.
// defaultBranch names the branch it may not list, as the help text reads it;
// older says which pipelines ignore the setting.
func branchesParam(defaultBranch, unmapped, older string) Param {
	return Param{Key: ParamBranches, Label: "Branch environments", Kind: KindString, Advanced: true, Placeholder: "develop=dev release/*=qa",
		Help: "Other branches that deploy, as space-separated <branch>=<environment> pairs, for example develop=dev release/*=qa. " +
			"A branch is a name or a glob where * matches any characters, including /. Each environment must be one of spec.environments. " +
			defaultBranch + " always deploys into the production environment and is not listed; the first matching pair wins. " + unmapped + " " +
			older}
}

// environmentHelp completes the cicd providers' environment setting help.
const environmentHelp = "Apps publish at their url, else <id>.<domain>, there; in the other environments at the chart's environmentUrls.<environment>, else <id>-<environment>.<domain>."

// pullSecretParam is the images providers' pull secret name setting.
func pullSecretParam(def string) Param {
	return Param{Key: "pullSecretName", Label: "Pull secret name", Kind: KindString, Required: true, Default: def, Advanced: true,
		Help:    "Name of the image pull secret in each environment. Use harbor-robot for charts written for older Ownstack clusters.",
		Pattern: kubeNamePattern, PatternHelp: kubeNameHelp}
}

// Default returns the Ownstack catalog. Availability is filled in by the
// providers package from the implementations it actually registers.
func Default() *Catalog {
	return &Catalog{
		Version: Version,
		Aspects: []Aspect{
			{
				ID: "compute", Name: "Compute", Tag: "COMPUTE", Required: true,
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
				ID: "dns", Name: "DNS", Tag: "DNS", Required: true,
				Consumes: []string{"compute"},
				Contract: Contract{
					Summary: "The domain and its wildcard resolve to the node, and ingress can prove domain ownership for TLS.",
					Outputs: []Output{
						{Key: "records", Description: "<domain> and *.<domain> resolve to the compute public IP."},
						{Key: "acmeSolver", Description: "How certificates are validated: a DNS-01 provider with credentials, or TLS-ALPN on the node."},
					},
				},
			},
			{
				ID: "ingress", Name: "Ingress & TLS", Tag: "INGRESS", Required: true,
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
				ID: "vcs", Name: "Version control", Tag: "VCS",
				Consumes: []string{"compute"},
				Contract: Contract{
					Summary: "Hosts the customer-owned cluster repo and application repos, with a credential CI can clone with.",
					Outputs: []Output{
						{Key: "owner", Description: "Account or organization that owns the repos."},
						{Key: "clusterRepo", Description: "Name and clone URL of the cluster repo (pipeline and Helm libraries)."},
						{Key: "demoRepo", Description: "Demo application repo, when the stack has one."},
						{Key: "credential", Description: "Username and token for cloning and repository discovery."},
					},
				},
			},
			{
				ID: "images", Name: "Image registry", Tag: "IMAGES",
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
				ID: "alerts", Name: "Alerting", Tag: "ALERTS",
				Consumes: []string{"compute"},
				Contract: Contract{
					Summary: "A notification sink for pipeline results and monitor alarms.",
					Outputs: []Output{
						{Key: "sink", Description: "Channel kind, destination, and credential."},
					},
				},
			},
			{
				ID: "metrics", Name: "Observability", Tag: "METRICS",
				Consumes: []string{"compute", "ingress", "alerts"},
				Contract: Contract{
					Summary: "A metrics store, dashboards, and a query endpoint alert rules evaluate against.",
					Outputs: []Output{
						{Key: "queryEndpoint", Description: "PromQL-compatible query endpoint."},
						{Key: "dashboards", Description: "Dashboard URL."},
					},
				},
			},
			{
				ID: "cicd", Name: "CI/CD", Tag: "CI/CD",
				Consumes: []string{"compute", "ingress", "vcs", "images", "alerts"},
				Contract: Contract{
					Summary: "Discovers application repos, builds images, pushes them, and deploys Helm charts to the environments.",
					Outputs: []Output{
						{Key: "url", Description: "CI web UI."},
						{Key: "pipelineConvention", Description: "infrastructure/Jenkinsfile, infrastructure/<app>/Dockerfile and infrastructure/<app>/helm."},
					},
				},
			},
		},
		Providers: []Provider{
			// ── compute ────────────────────────────────────────────────
			{
				ID: "own-vps", Aspect: "compute", Name: "Own VPS", Mono: "VPS", Hosting: SelfHosted, Recommended: true,
				Summary:   "Any Ubuntu server: bare metal, homelab, or a VPS from any host. k3s is installed on it and ownstackctl runs on the node itself.",
				Resources: Resources{MemoryMiB: 800, DiskGiB: 10},
				Params: []Param{
					{Key: "host", Label: "Public IP", Kind: KindIP, Required: true, Placeholder: "203.0.113.10",
						Help: "Public IPv4 address of the server. DNS records point here."},
					{Key: "swapGiB", Label: "Swap (GiB)", Kind: KindInt, Default: "2", Advanced: true,
						Help: "Swapfile created when the server has no swap, so a build spike does not take services down. 0 disables."},
					{Key: "k3sChannel", Label: "k3s channel", Kind: KindString, Default: "stable", Advanced: true},
					{Key: "k3sVersion", Label: "k3s version", Kind: KindString, Advanced: true,
						Help:    "Pin an exact k3s release, for example v1.33.4+k3s1. Overrides the channel.",
						Pattern: `^v?[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?(\+k3s[0-9]+)?$`, PatternHelp: "must be a k3s release such as v1.33.4+k3s1"},
				},
				Paths: []Path{
					{ID: "cli-agent", Name: "CLI agent", Recommended: true,
						Description: "Run ownstackctl on the server yourself. Secrets are entered on the server and never reach Ownstack."},
					{ID: "ssh-key", Name: "SSH key",
						Description: "Give Ownstack a root SSH key and it runs ownstackctl for you. Secrets pass through Ownstack encrypted."},
				},
			},
			{ID: "hetzner", Aspect: "compute", Name: "Hetzner Cloud", Mono: "HZ", Hosting: SelfHosted,
				Summary: "Servers created through the Hetzner API. The hardware is rented; everything on top is yours."},
			{ID: "ec2", Aspect: "compute", Name: "AWS EC2", Mono: "EC2", Hosting: SelfHosted,
				Summary: "Instances created in your AWS account."},

			// ── dns ────────────────────────────────────────────────────
			{
				ID: "cloudflare", Aspect: "dns", Name: "Cloudflare", Mono: "CF", Hosting: External, Recommended: true,
				Summary:  "A records are managed through the Cloudflare API, and certificates use DNS-01 so no ports need to be reachable during issuance.",
				Requires: []string{"compute"},
				Params: []Param{
					{Key: "apiToken", Label: "API token", Kind: KindSecret, Required: true, Env: "CLOUDFLARE_API_TOKEN",
						Help:    "Use the \"Edit zone DNS\" template, limited to this domain's zone.",
						HelpURL: "https://dash.cloudflare.com/profile/api-tokens"},
					{Key: "zone", Label: "Zone", Kind: KindHostname, Advanced: true,
						Help: "Cloudflare zone name. Detected from the domain when empty."},
					{Key: "replaceRecords", Label: "Replace existing records", Kind: KindBool, Default: "false", Advanced: true,
						Help: "Let ownstackctl replace records for the domain and its wildcard that it did not create. When off, apply stops instead of overwriting them."},
				},
			},
			{
				ID: "manual", Aspect: "dns", Name: "Manual DNS", Mono: "DNS", Hosting: SelfHosted,
				Summary:  "Create A records for the domain and its wildcard at any DNS host yourself. ownstackctl waits until they resolve, and certificates are issued per hostname over TLS-ALPN.",
				Requires: []string{"compute"},
				Params: []Param{
					{Key: "waitMinutes", Label: "Wait for records", Kind: KindInt, Default: "30", Advanced: true,
						Help: "How long apply waits for the records to resolve before failing."},
				},
			},
			{ID: "route53", Aspect: "dns", Name: "Route 53", Mono: "R53", Hosting: External,
				Summary: "Records managed in an AWS Route 53 hosted zone."},

			// ── ingress ────────────────────────────────────────────────
			{
				ID: "traefik", Aspect: "ingress", Name: "Traefik", Mono: "TR", Hosting: SelfHosted, Recommended: true,
				Summary:   "Traefik ingress with Let's Encrypt certificates and a dashboard.",
				Requires:  []string{"compute", "dns"},
				Resources: Resources{MemoryMiB: 128, DiskGiB: 1},
				Params: []Param{
					{Key: "acmeEmail", Label: "ACME email", Kind: KindEmail, Required: true, Placeholder: "ops@example.com",
						Help: "Let's Encrypt account email for expiry notices."},
					{Key: "dashboardHost", Label: "Dashboard host", Kind: KindHostname, Default: "traefik.{adminDomain}"},
					{Key: "acmeStaging", Label: "Use Let's Encrypt staging", Kind: KindBool, Default: "false", Advanced: true,
						Help: "Issue untrusted staging certificates. Useful for test runs that would hit rate limits."},
					{Key: "chartVersion", Label: "Chart version", Kind: KindString, Default: "36.3.0", Advanced: true},
				},
			},

			// ── vcs ────────────────────────────────────────────────────
			{
				ID: "github", Aspect: "vcs", Name: "GitHub", Mono: "GH", Hosting: External, Recommended: true,
				Summary:  "Repositories on GitHub. The cluster repo is generated from the Ownstack template into your account.",
				Requires: []string{"compute"},
				Params: []Param{
					{Key: "owner", Label: "Owner", Kind: KindString, Required: true, Placeholder: "your-user-or-org",
						Help: "GitHub user or organization that owns the cluster and application repos."},
					{Key: "token", Label: "Access token", Kind: KindSecret, Required: true, Env: "GITHUB_TOKEN",
						Help:    "Classic token with the repo and workflow scopes (pre-selected by the link).",
						HelpURL: "https://github.com/settings/tokens/new?scopes=repo,workflow&description=Ownstack"},
					{Key: "clusterRepo", Label: "Cluster repo", Kind: KindString, Default: "ownstack-cluster"},
					{Key: "demoApp", Label: "Create demo app repo", Kind: KindBool, Default: "true", Advanced: true,
						Help: "Generate the Lumen Yard demo app so the first pipeline run has something to deploy."},
				},
			},
			{ID: "gitea", Aspect: "vcs", Name: "Gitea", Mono: "GT", Hosting: SelfHosted,
				Summary: "A lightweight forge running inside your cluster."},
			{ID: "gitlab", Aspect: "vcs", Name: "GitLab.com", Mono: "GL", Hosting: External,
				Summary: "Hosted GitLab repositories."},

			// ── images ─────────────────────────────────────────────────
			{
				ID: "harbor", Aspect: "images", Name: "Harbor", Mono: "HB", Hosting: SelfHosted,
				Summary:   "A full private registry in your cluster, with projects and robot accounts.",
				Requires:  []string{"compute", "ingress"},
				Resources: Resources{MemoryMiB: 1500, DiskGiB: 20},
				Params: []Param{
					{Key: "host", Label: "Host", Kind: KindHostname, Default: "harbor.{adminDomain}"},
					{Key: "adminPassword", Label: "Admin password", Kind: KindSecret, Required: true, Generate: true, Env: "HARBOR_ADMIN_PASSWORD",
						Help: "Generated on the server when not provided."},
					{Key: "project", Label: "Project", Kind: KindString, Default: "product", Advanced: true,
						Pattern: imagePathPattern, PatternHelp: imagePathHelp},
					pullSecretParam("harbor-robot"),
					{Key: "chartVersion", Label: "Chart version", Kind: KindString, Default: "1.17.2", Advanced: true},
				},
			},
			{
				ID: "zot", Aspect: "images", Name: "Zot", Mono: "ZT", Hosting: SelfHosted, Recommended: true,
				Summary:   "A minimal OCI registry in your cluster: one small process, password-protected, on a persistent volume. The light alternative to Harbor.",
				Requires:  []string{"compute", "ingress"},
				Resources: Resources{MemoryMiB: 100, DiskGiB: 10},
				Params: []Param{
					{Key: "host", Label: "Host", Kind: KindHostname, Default: "registry.{adminDomain}"},
					{Key: "username", Label: "Username", Kind: KindString, Default: "ownstack",
						Pattern: `^[^:\x00-\x20\x7f]+$`, PatternHelp: "must not contain ':', spaces, or control characters"},
					{Key: "password", Label: "Password", Kind: KindSecret, Required: true, Generate: true, Env: "ZOT_PASSWORD",
						Help: "Generated on the server when not provided. Use it to docker login."},
					{Key: "project", Label: "Image namespace", Kind: KindString, Default: "apps", Advanced: true,
						Pattern: imagePathPattern, PatternHelp: imagePathHelp},
					{Key: "storageGiB", Label: "Storage (GiB)", Kind: KindInt, Default: "10", Advanced: true},
					{Key: "keepTags", Label: "Tags to keep", Kind: KindInt, Default: "0", Advanced: true,
						Help:    "How many of the most recently pushed tags to keep in each repository; older tags are deleted unless pulled in the last 30 days. 0 keeps everything.",
						Pattern: `^[0-9]+$`, PatternHelp: "must be 0 or more"},
					pullSecretParam("registry-credentials"),
					{Key: "chartVersion", Label: "Chart version", Kind: KindString, Default: "0.1.128", Advanced: true},
				},
			},
			{
				ID: "registry", Aspect: "images", Name: "Existing registry", Mono: "REG", Hosting: External,
				Summary:  "Use a registry you already run or rent: your own Harbor, GitHub Container Registry, Docker Hub, or any OCI registry. Nothing is installed; Ownstack only wires its credential into CI and the environments.",
				Requires: []string{"compute"},
				Params: []Param{
					{Key: "host", Label: "Registry host", Kind: KindHostname, Required: true, Placeholder: "harbor.example.com",
						Help: "For example harbor.yourcompany.com, ghcr.io, or docker.io."},
					{Key: "project", Label: "Project / namespace", Kind: KindString, Required: true, Placeholder: "product",
						Help:    "Path images are pushed under: a Harbor project, your GitHub user for ghcr.io, or your Docker Hub user.",
						Pattern: imagePathPattern, PatternHelp: imagePathHelp},
					{Key: "username", Label: "Username", Kind: KindString, Required: true,
						Help: "A robot or service account is best (Harbor robot, GitHub user, Docker Hub user)."},
					{Key: "password", Label: "Password or token", Kind: KindSecret, Required: true, Env: "REGISTRY_PASSWORD",
						Help: "Needs push and pull on the project. For ghcr.io use a token with write:packages."},
					pullSecretParam("registry-credentials"),
				},
			},

			// ── alerts ─────────────────────────────────────────────────
			{
				ID: "slack", Aspect: "alerts", Name: "Slack", Mono: "SL", Hosting: External,
				Summary:  "Pipeline results posted to a Slack channel by a bot.",
				Requires: []string{"compute"},
				Params: []Param{
					{Key: "channel", Label: "Channel", Kind: KindString, Required: true, Default: "#deployments"},
					{Key: "token", Label: "Bot token", Kind: KindSecret, Required: true, Env: "SLACK_BOT_TOKEN",
						Help:    "Bot user OAuth token (xoxb-...) with chat:write, invited to the channel.",
						HelpURL: "https://api.slack.com/apps"},
				},
			},
			{ID: "telegram", Aspect: "alerts", Name: "Telegram", Mono: "TG", Hosting: External,
				Summary: "Alerts delivered to a Telegram chat through a bot."},
			{ID: "ntfy", Aspect: "alerts", Name: "ntfy", Mono: "NT", Hosting: SelfHosted,
				Summary: "Self-hosted push notifications."},

			// ── metrics ────────────────────────────────────────────────
			{ID: "grafana", Aspect: "metrics", Name: "Grafana stack", Mono: "GF", Hosting: SelfHosted,
				Summary:   "Prometheus, Loki, and Grafana in your cluster.",
				Resources: Resources{MemoryMiB: 1500, DiskGiB: 20}},
			{ID: "kuma", Aspect: "metrics", Name: "Uptime Kuma", Mono: "UK", Hosting: SelfHosted,
				Summary: "Simple self-hosted uptime monitoring."},
			{ID: "datadog", Aspect: "metrics", Name: "Datadog", Mono: "DD", Hosting: External,
				Summary: "Hosted metrics, logs, and APM."},

			// ── cicd ───────────────────────────────────────────────────
			{
				ID: "jenkins", Aspect: "cicd", Name: "Jenkins", Mono: "JK", Hosting: SelfHosted,
				Summary:   "Jenkins with Kubernetes pod agents and the shared Ownstack pipeline library from your cluster repo.",
				Requires:  []string{"compute", "ingress", "vcs", "images"},
				Resources: Resources{MemoryMiB: 1500, DiskGiB: 10},
				Params: []Param{
					{Key: "host", Label: "Host", Kind: KindHostname, Default: "jenkins.{adminDomain}"},
					{Key: "adminPassword", Label: "Admin password", Kind: KindSecret, Required: true, Generate: true, Env: "JENKINS_ADMIN_PASSWORD",
						Help: "Generated on the server when not provided."},
					{Key: "orgFolder", Label: "Organization folder", Kind: KindString, Default: "Repositories", Advanced: true},
					{Key: "repoFilter", Label: "Repository filter", Kind: KindString, Default: "*", Advanced: true,
						Help: "Space-separated wildcards, such as app-* api-*. Matching repos with a Jenkinsfile get a pipeline."},
					{Key: "jenkinsfilePath", Label: "Jenkinsfile path", Kind: KindString, Default: "infrastructure/Jenkinsfile", Advanced: true},
					{Key: ParamEnvironment, Label: "Production environment", Kind: KindEnvironment, Required: true, Default: "prod", Advanced: true,
						Help: "Environment the deploy branch deploys into, exported to Jenkins as OWNSTACK_NAMESPACE. " + environmentHelp + " Cluster repos with an older pipeline library always deploy to prod."},
					{Key: ParamDeployBranch, Label: "Deploy branch", Kind: KindString, Required: true, Default: DefaultBranch, Advanced: true,
						Help:    "Branch the organization folder builds automatically and the pipeline pushes and deploys into the production environment, exported as OWNSTACK_DEPLOY_BRANCH. Other branches build only unless Branch environments maps them.",
						Pattern: `^[A-Za-z0-9][A-Za-z0-9._/-]*$`, PatternHelp: "must be a branch name of letters, digits, '.', '_', '-' and '/'"},
					branchesParam("The deploy branch", "Branches that match no pair build only.", "Cluster repos with an older pipeline library deploy only the deploy branch."),
					{Key: "chartVersion", Label: "Chart version", Kind: KindString, Default: JenkinsChartVersion, Advanced: true,
						Help: "jenkins/jenkins Helm chart. Each ownstackctl release locks Jenkins and every plugin for its default chart; with another chart the locked plugins may not start."},
					{Key: "extraPlugins", Label: "Extra plugins", Kind: KindString, Advanced: true, Placeholder: "build-timeout:1.41 ansicolor:542.v03d235fee02d",
						Help:    "Plugins to add to the locked set, as space-separated name:version pairs; versions are required so installs stay reproducible. A locked plugin listed here gets this version instead. Dependencies missing from the locked set are installed at the oldest version the plugin accepts, so list them too.",
						Pattern: jenkinsPluginsPattern, PatternHelp: "must be space-separated name:version pairs, such as build-timeout:1.41"},
				},
			},
			{ID: "woodpecker", Aspect: "cicd", Name: "Woodpecker CI", Mono: "WP", Hosting: SelfHosted,
				Summary: "A lightweight container-native CI in your cluster."},
			{
				ID: "gh-actions", Aspect: "cicd", Name: "GitHub Actions", Mono: "GA", Hosting: External, Recommended: true,
				Summary:  "Builds run on GitHub's runners, so the server only runs your apps. Each app repo gets a small ownstack.yml workflow that builds, pushes to your registry, and deploys with a namespace-scoped credential.",
				Requires: []string{"compute", "ingress", "vcs", "images"},
				Params: []Param{
					{Key: "repoFilter", Label: "Repository filter", Kind: KindString, Default: "*", Advanced: true,
						Help: "Space-separated wildcards, such as app-* api-*. Matching repos that contain .github/workflows/ownstack.yml get the deploy credential."},
					{Key: "libraryRef", Label: "Ownstack library ref", Kind: KindString, Advanced: true,
						Help: "Branch or tag of getownstack/ownstack-cluster-template the workflow takes the Helm library from. Empty: the ownstackctl release running on the server, or main for development builds."},
					{Key: ParamEnvironment, Label: "Production environment", Kind: KindEnvironment, Required: true, Default: "prod", Advanced: true,
						Help: "Environment that pushes to main deploy into. " + environmentHelp},
					branchesParam(DefaultBranch, "Pushes to branches that match no pair do not deploy.", "App repos with a workflow older than ownstack-workflow 3 deploy only main."),
					{Key: "additionalOwners", Label: "Additional owners", Kind: KindString, Advanced: true,
						Help: "Comma-separated GitHub users or organizations whose repos also deploy here. The token must be able to administer them."},
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
			{
				ID: "byo-registry", Name: "Bring your registry", MinMemoryGB: 2,
				Summary: "Already run Harbor, or use GHCR or Docker Hub? Point Ownstack at it; builds on GitHub Actions.",
				Components: map[string]string{
					"compute": "own-vps", "dns": "cloudflare", "ingress": "traefik",
					"vcs": "github", "images": "registry", "cicd": "gh-actions",
				},
			},
		},
	}
}
