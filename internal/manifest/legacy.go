package manifest

import (
	"strings"
)

// FromLegacyEnv builds a manifest from the environment contract used by
// Ownstack before ownstack.yaml existed (lowercase variables such as vps,
// base_domain, github_pat). It returns the manifest and the secret values
// keyed by their conventional environment variable names. ok is false when
// the legacy contract is not present.
func FromLegacyEnv(getenv func(string) string) (m *Manifest, secrets map[string]string, ok bool) {
	if getenv("vps") == "" || getenv("base_domain") == "" {
		return nil, nil, false
	}

	domain := strings.TrimSpace(getenv("base_domain"))
	domain = strings.TrimPrefix(domain, "*.")
	domain = strings.TrimPrefix(domain, "https://")
	domain = strings.TrimPrefix(domain, "http://")
	domain, _, _ = strings.Cut(domain, "/")

	clusterRepo := ""
	if repo := getenv("system_root_app_repo"); repo != "" {
		repo = strings.TrimSuffix(strings.TrimSuffix(repo, "/"), ".git")
		clusterRepo = repo[strings.LastIndex(repo, "/")+1:]
	}
	if clusterRepo == "" {
		clusterRepo = getenv("cluster_repo_name")
	}

	settings := func(pairs ...string) map[string]string {
		out := map[string]string{}
		for i := 0; i+1 < len(pairs); i += 2 {
			if v := getenv(pairs[i+1]); v != "" {
				out[pairs[i]] = v
			}
		}
		return out
	}

	m = &Manifest{
		APIVersion: APIVersion,
		Kind:       Kind,
		Metadata:   Metadata{Name: "ownstack"},
		Spec: Spec{
			Domain: domain,
			Components: map[string]*Component{
				"compute": {Provider: "own-vps", Settings: settings("host", "vps")},
				"dns":     {Provider: "cloudflare"},
				"ingress": {Provider: "traefik", Settings: settings(
					"acmeEmail", "traefik_email",
					"dashboardHost", "traefik_dashboard",
					"chartVersion", "traefik_chart_version",
				)},
				"vcs": {Provider: "github", Settings: settings("owner", "git_username")},
				"images": {Provider: "harbor", Settings: settings(
					"host", "harbor_hostname",
					"chartVersion", "harbor_chart_version",
				)},
				"cicd": {Provider: "jenkins", Settings: settings(
					"host", "jenkins_hostname",
					"chartVersion", "jenkins_chart_version",
					"orgFolder", "jenkins_github_org_folder_name",
					"repoFilter", "jenkins_github_org_folder_repo_filter",
					"jenkinsfilePath", "jenkins_jenkinsfile_path",
				)},
			},
		},
	}
	if clusterRepo != "" {
		m.Spec.Components["vcs"].Settings["clusterRepo"] = clusterRepo
	}

	secrets = map[string]string{}
	for env, legacy := range map[string]string{
		"CLOUDFLARE_API_TOKEN":   "cloudflare_token",
		"GITHUB_TOKEN":           "github_pat",
		"HARBOR_ADMIN_PASSWORD":  "harbor_initial_password",
		"JENKINS_ADMIN_PASSWORD": "jenkins_initial_password",
	} {
		if v := getenv(legacy); v != "" {
			secrets[env] = v
		}
	}
	return m, secrets, true
}
