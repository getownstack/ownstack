// Package stack holds the types shared by the engine and provider
// implementations: the runtime a provider runs in, the Provider interface,
// and the typed contract outputs aspects hand to each other.
package stack

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/getownstack/ownstack/internal/catalog"
	"github.com/getownstack/ownstack/internal/manifest"
	"github.com/getownstack/ownstack/internal/secrets"
)

// Provider implements one aspect's contract.
//
// Resolve fills the provider's statically known contract outputs (hostnames,
// names, credentials) without touching infrastructure, so status can run
// without applying. Apply converges the node towards the manifest and may
// complete outputs only known after installation. Every step must be safe to
// rerun.
type Provider interface {
	Resolve(ctx context.Context, rt *Runtime) error
	Apply(ctx context.Context, rt *Runtime) error
	Status(ctx context.Context, rt *Runtime) []Check
}

// Check states.
const (
	OK      = "ok"
	Warn    = "warn"
	Fail    = "fail"
	Unknown = "unknown"
)

type Check struct {
	Name   string `json:"name"`
	State  string `json:"state"`
	Detail string `json:"detail,omitempty"`
}

// Runtime is everything a provider needs to resolve, apply, and inspect.
type Runtime struct {
	Manifest *manifest.Manifest
	Catalog  *catalog.Catalog
	Secrets  *secrets.Store
	Console  *Console
	Exec     *Exec
	Kube     *Kube
	HTTP     *http.Client
	// ServiceHTTP overrides ServiceClient in tests.
	ServiceHTTP *http.Client
	Out         *Outputs

	aspect string
}

func NewRuntime(m *manifest.Manifest, cat *catalog.Catalog, store *secrets.Store, console *Console) *Runtime {
	exec := &Exec{Console: console, Env: []string{"KUBECONFIG=" + Kubeconfig}}
	return &Runtime{
		Manifest: m,
		Catalog:  cat,
		Secrets:  store,
		Console:  console,
		Exec:     exec,
		Kube:     &Kube{Exec: exec},
		HTTP:     &http.Client{Timeout: 30 * time.Second},
		Out:      &Outputs{},
	}
}

// Kubeconfig is where k3s writes its admin kubeconfig.
const Kubeconfig = "/etc/rancher/k3s/k3s.yaml"

// StateDir is where ownstackctl keeps a node's state: $OWNSTACK_DIR, by
// default /etc/ownstack. It holds ownstack.yaml (the last applied manifest),
// secrets.env, apply.lock, health.env, and charts/.
func StateDir() string {
	if dir := os.Getenv("OWNSTACK_DIR"); dir != "" {
		return dir
	}
	return "/etc/ownstack"
}

// StateFile is a file in StateDir.
func StateFile(name string) string { return filepath.Join(StateDir(), name) }

// For scopes the runtime to one aspect so Setting and Secret read that
// aspect's component.
func (rt *Runtime) For(aspect string) *Runtime {
	scoped := *rt
	scoped.aspect = aspect
	return &scoped
}

// param is one of the aspect's provider's params. Keys are constants in
// provider code, so a key the provider's Spec does not declare (or declares
// as the other kind) is a bug that would otherwise read nothing: it panics,
// and the provider's tests catch it.
func (rt *Runtime) param(key string, secret bool) (catalog.Provider, catalog.Param) {
	p, _ := rt.Catalog.Provider(rt.aspect, rt.Manifest.Provider(rt.Catalog, rt.aspect))
	param, ok := p.Param(key)
	if !ok || param.IsSecret() != secret {
		what := "setting"
		if secret {
			what = "secret"
		}
		panic(fmt.Sprintf("the %s provider %q declares no %s %q", rt.aspect, p.ID, what, key))
	}
	return p, param
}

// Setting is one of this aspect's settings, with its default applied.
func (rt *Runtime) Setting(key string) string {
	rt.param(key, false)
	return rt.Manifest.Settings(rt.Catalog, rt.aspect)[key]
}

func (rt *Runtime) SettingBool(key string) bool {
	v, _ := strconv.ParseBool(rt.Setting(key))
	return v
}

func (rt *Runtime) SettingInt(key string) int {
	v, _ := strconv.Atoi(rt.Setting(key))
	return v
}

// Values returns the manifest's Helm value overrides for this aspect.
func (rt *Runtime) Values() map[string]any { return rt.Manifest.Values(rt.aspect) }

// Secret resolves one of this aspect's secrets from its environment
// variable, the node's secrets file, generation, or a prompt (package
// secrets).
func (rt *Runtime) Secret(key string) (string, error) {
	p, param := rt.param(key, true)
	return rt.Secrets.Get(secrets.Request{Env: param.Env, Label: p.Label(param), Help: param.Help, Generate: param.Generate})
}

func (rt *Runtime) Logf(format string, args ...any) { rt.Console.Printf(format, args...) }

// ── Contract outputs ─────────────────────────────────────────────────────────
// Each aspect's provider fills its output; consumers read only these types,
// never another provider's settings.

type Outputs struct {
	Compute *ComputeOutput
	DNS     *DNSOutput
	Ingress *IngressOutput
	VCS     *VCSOutput
	Images  *ImagesOutput
	CICD    *CICDOutput
}

type ComputeOutput struct {
	PublicIP string
	// APIServer is the cluster API URL reachable from outside the node, for
	// CI systems that deploy remotely; CACert is its PEM CA bundle.
	APIServer string
	CACert    string
	// Arch is the node's CPU architecture as a GOARCH value (amd64, arm64).
	Arch string
}

// DNSOutput is how ingress proves domain ownership: DNS-01 through a lego
// provider, with the environment that provider needs.
type DNSOutput struct {
	ACMEProvider string
	ACMEEnv      map[string]string
}

type IngressOutput struct {
	// Annotations make a standard Kubernetes Ingress use this ingress over HTTPS.
	Annotations map[string]string
	// UntrustedCerts is set when certificates are issued by a CA that clients
	// do not trust by default, such as Let's Encrypt staging. HTTPS checks
	// then accept them.
	UntrustedCerts bool
	// DashboardURL is the ingress controller's dashboard, when it has one.
	DashboardURL string
}

type VCSOutput struct {
	Kind   string // github
	Owner  string
	APIURL string
	// ClusterRepoURL is the web URL of the repo that mirrors ownstack.yaml.
	ClusterRepoURL string
	// DemoRepo is the demo application repo under Owner; empty when the
	// stack has no demo app.
	DemoRepo string
	Username string
	Token    string
}

type ImagesOutput struct {
	Host         string
	Project      string
	PushUsername string
	PushPassword string
	// Login tells a person how to sign in to the registry's web UI at
	// https://<Host>, for the run summary; empty when it has none. It names
	// where the password is kept, never the value.
	Login string
}

type CICDOutput struct {
	URL string
	// Login tells a person how to sign in at URL, for the run summary; empty
	// when no separate login is needed. Never a secret value.
	Login string
}

// ── Stack-wide settings ──────────────────────────────────────────────────────
// Providers read the stack's own settings through these, never through
// rt.Manifest (internal/providers/contract_test.go enforces it).

// Domain is where applications are published: <app>.<domain>.
func (rt *Runtime) Domain() string { return rt.Manifest.Domain }

// AdminDomain is the parent domain of platform service hosts.
func (rt *Runtime) AdminDomain() string { return rt.Manifest.AdminDomain() }

// AdminHost is a platform service's host: <svc>.<adminDomain>.
func (rt *Runtime) AdminHost(svc string) string { return svc + "." + rt.AdminDomain() }

// Environment is one deploy environment: a namespace, and the branches
// (a name, or a glob where * matches any characters, / included) that
// deploy into it. Branch is empty when no branch deploys there.
type Environment = manifest.Environment

// Environments are the stack's environments in manifest order. The first is
// production, and a branch deploys into the first one whose glob matches it.
func (rt *Runtime) Environments() []Environment { return rt.Manifest.EnvironmentList() }

// Production is the production environment, the first one.
func (rt *Runtime) Production() Environment { return rt.Environments()[0] }

// ManifestFile is ownstack.yaml as it was applied, for the cluster repo that
// mirrors it.
func (rt *Runtime) ManifestFile() []byte { return rt.Manifest.Source }
