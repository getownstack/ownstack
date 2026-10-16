// Package stack holds the types shared by the engine and provider
// implementations: the runtime a provider runs in, the Provider interface,
// and the typed contract outputs aspects hand to each other.
package stack

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/getownstack/ownstack-cluster-template/internal/catalog"
	"github.com/getownstack/ownstack-cluster-template/internal/manifest"
	"github.com/getownstack/ownstack-cluster-template/internal/secrets"
)

// Provider implements one aspect's contract.
//
// Resolve fills the provider's statically known contract outputs (hostnames,
// names, credentials) without touching infrastructure, so plan and status can
// run without applying. Apply converges the node towards the manifest and may
// complete outputs only known after installation. Every step must be safe to
// rerun.
type Provider interface {
	Resolve(ctx context.Context, rt *Runtime) error
	Plan(ctx context.Context, rt *Runtime) []Step
	Apply(ctx context.Context, rt *Runtime) error
	Status(ctx context.Context, rt *Runtime) []Check
}

// Step is one planned action.
type Step struct {
	Action string `json:"action"` // install, upgrade, configure, ensure, verify
	Target string `json:"target"`
	Detail string `json:"detail,omitempty"`
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

// Runtime is everything a provider needs to plan, apply, and inspect.
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

// For scopes the runtime to one aspect so Setting and Secret read that
// aspect's component.
func (rt *Runtime) For(aspect string) *Runtime {
	scoped := *rt
	scoped.aspect = aspect
	return &scoped
}

func (rt *Runtime) Aspect() string { return rt.aspect }

func (rt *Runtime) Setting(key string) string {
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
func (rt *Runtime) Values() map[string]any {
	if c, ok := rt.Manifest.Component(rt.aspect); ok {
		return c.Values
	}
	return nil
}

// Secret resolves one of this aspect's secrets.
func (rt *Runtime) Secret(key string) (string, error) {
	c, _ := rt.Manifest.Component(rt.aspect)
	req := secrets.Request{Ref: rt.Manifest.SecretRef(rt.Catalog, rt.aspect, key), Label: key}
	if c != nil {
		if p, ok := rt.Catalog.Provider(rt.aspect, c.Provider); ok {
			if param, ok := p.Param(key); ok {
				req.Label = p.Name + " " + lowerFirst(param.Label)
				req.Help = param.Help
				req.Generate = param.Generate
			}
		}
	}
	return rt.Secrets.Get(req)
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
	Alerts  *AlertsOutput
	CICD    *CICDOutput
}

type ComputeOutput struct {
	PublicIP     string
	Kubeconfig   string
	StorageClass string
}

// ACME challenge kinds.
const (
	ChallengeDNS     = "dns"
	ChallengeTLSALPN = "tls-alpn"
)

type DNSOutput struct {
	Challenge string
	// For DNS-01: the lego provider name and the environment it needs.
	ACMEProvider string
	ACMEEnv      map[string]string
}

type IngressOutput struct {
	EntryPoint   string
	CertResolver string
	// Annotations make a standard Kubernetes Ingress use this ingress over HTTPS.
	Annotations map[string]string
}

type VCSOutput struct {
	Kind           string // github
	Owner          string
	APIURL         string
	ClusterRepoURL string // https clone URL
	Username       string
	Token          string
}

type ImagesOutput struct {
	Host           string
	Project        string
	PushUsername   string
	PushPassword   string
	PullSecretName string
}

type AlertsOutput struct {
	Kind    string // slack
	Channel string
	Token   string
}

type CICDOutput struct {
	URL string
}

// lowerFirst lowercases a leading capital unless it starts an acronym.
func lowerFirst(s string) string {
	if len(s) > 1 && s[0] >= 'A' && s[0] <= 'Z' && s[1] >= 'a' && s[1] <= 'z' {
		return string(s[0]+('a'-'A')) + s[1:]
	}
	return s
}
