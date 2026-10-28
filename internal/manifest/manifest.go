// Package manifest reads, validates, and writes ownstack.yaml.
//
// ownstack.yaml is the declarative definition of a stack. It lives in the
// customer's cluster repo and ownstackctl reconciles the node towards it.
// Secret values never live in the manifest; it only holds references
// ("env:NAME" or "file:/path"), and ownstackctl resolves them on the node.
package manifest

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"net/mail"
	"os"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/getownstack/ownstack/internal/catalog"
)

const (
	APIVersion = "ownstack.io/v1"
	Kind       = "Stack"
	FileName   = "ownstack.yaml"

	DefaultAdminSubdomain = "admin"
)

var DefaultEnvironments = []string{"dev", "qa", "prod"}

// Environments become namespaces that CI deploy credentials manage, so they
// must not be Kubernetes' own namespaces or those of platform services.
var (
	reservedEnvironments        = []string{"default", "traefik", "zot", "harbor", "jenkins"}
	reservedEnvironmentPrefixes = []string{"kube-", "ownstack-"}
)

// ReservedEnvironment reports whether name may not be used as an environment:
// default, kube-*, ownstack-*, or a platform service's namespace.
func ReservedEnvironment(name string) bool {
	if slices.Contains(reservedEnvironments, name) {
		return true
	}
	for _, prefix := range reservedEnvironmentPrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// ErrNotV1 is returned for files that are not ownstack.io/v1 manifests, such
// as the design metadata written by early versions of the Ownstack designer.
var ErrNotV1 = errors.New("not an ownstack.io/v1 manifest")

type Manifest struct {
	APIVersion string   `yaml:"apiVersion"`
	Kind       string   `yaml:"kind"`
	Metadata   Metadata `yaml:"metadata"`
	Spec       Spec     `yaml:"spec"`
}

type Metadata struct {
	Name string `yaml:"name"`
}

type Spec struct {
	Domain string `yaml:"domain"`
	// AdminSubdomain places platform services under <svc>.<adminSubdomain>.<domain>.
	// nil means the default; an empty string places them directly under the domain.
	AdminSubdomain *string               `yaml:"adminSubdomain,omitempty"`
	Environments   []string              `yaml:"environments,omitempty"`
	Components     map[string]*Component `yaml:",inline"`
}

type Component struct {
	Provider string            `yaml:"provider"`
	Settings map[string]string `yaml:"settings,omitempty"`
	Secrets  map[string]string `yaml:"secrets,omitempty"`
	// Values are extra Helm values merged over the provider's defaults.
	Values map[string]any `yaml:"values,omitempty"`
}

func Parse(data []byte) (*Manifest, error) {
	var probe struct {
		APIVersion string `yaml:"apiVersion"`
	}
	if err := yaml.Unmarshal(data, &probe); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	if probe.APIVersion != APIVersion {
		return nil, ErrNotV1
	}
	var m Manifest
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	if m.Spec.Components == nil {
		m.Spec.Components = map[string]*Component{}
	}
	for id, c := range m.Spec.Components {
		if c == nil {
			delete(m.Spec.Components, id)
		}
	}
	return &m, nil
}

func Load(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

func (m *Manifest) AdminSubdomain() string {
	if m.Spec.AdminSubdomain == nil {
		return DefaultAdminSubdomain
	}
	return *m.Spec.AdminSubdomain
}

// AdminDomain is the parent domain of platform service hostnames.
func (m *Manifest) AdminDomain() string {
	if sub := m.AdminSubdomain(); sub != "" {
		return sub + "." + m.Spec.Domain
	}
	return m.Spec.Domain
}

func (m *Manifest) Environments() []string {
	if len(m.Spec.Environments) == 0 {
		return DefaultEnvironments
	}
	return m.Spec.Environments
}

func (m *Manifest) Component(aspect string) (*Component, bool) {
	c, ok := m.Spec.Components[aspect]
	return c, ok && c != nil
}

// Settings returns a component's settings with catalog defaults applied.
func (m *Manifest) Settings(cat *catalog.Catalog, aspect string) map[string]string {
	out := map[string]string{}
	c, ok := m.Component(aspect)
	if !ok {
		return out
	}
	if p, ok := cat.Provider(aspect, c.Provider); ok {
		for _, param := range p.Params {
			if !param.IsSecret() && param.Default != "" {
				out[param.Key] = m.expand(param.Default)
			}
		}
	}
	for k, v := range c.Settings {
		out[k] = v
	}
	return out
}

func (m *Manifest) expand(value string) string {
	return strings.NewReplacer("{domain}", m.Spec.Domain, "{adminDomain}", m.AdminDomain()).Replace(value)
}

// SecretRef returns where a component's secret is read from.
func (m *Manifest) SecretRef(cat *catalog.Catalog, aspect, key string) string {
	if c, ok := m.Component(aspect); ok {
		if ref, ok := c.Secrets[key]; ok && ref != "" {
			return ref
		}
		if p, ok := cat.Provider(aspect, c.Provider); ok {
			if param, ok := p.Param(key); ok && param.IsSecret() {
				return "env:" + param.Env
			}
		}
	}
	return ""
}

// ── Validation ────────────────────────────────────────────────────────────────

type Issue struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

func (i Issue) String() string { return i.Path + ": " + i.Message }

var (
	nameRe     = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	hostnameRe = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)
	envNameRe  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// Validate checks the manifest against the catalog. Only available providers
// pass: planned providers are part of the designer, not of an applicable stack.
func (m *Manifest) Validate(cat *catalog.Catalog) []Issue {
	var issues []Issue
	add := func(path, format string, args ...any) {
		issues = append(issues, Issue{Path: path, Message: fmt.Sprintf(format, args...)})
	}

	if m.APIVersion != APIVersion {
		add("apiVersion", "must be %s", APIVersion)
	}
	if m.Kind != Kind {
		add("kind", "must be %s", Kind)
	}
	if !nameRe.MatchString(m.Metadata.Name) {
		add("metadata.name", "must be a lowercase DNS label")
	}
	if !hostnameRe.MatchString(m.Spec.Domain) {
		add("spec.domain", "must be a domain name such as example.com")
	}
	if sub := m.AdminSubdomain(); sub != "" && !nameRe.MatchString(sub) {
		add("spec.adminSubdomain", "must be a lowercase DNS label or empty")
	}
	seenEnv := map[string]bool{}
	for i, env := range m.Environments() {
		path := fmt.Sprintf("spec.environments[%d]", i)
		if !nameRe.MatchString(env) {
			add(path, "must be a lowercase DNS label")
		} else if ReservedEnvironment(env) {
			add(path, "%q is reserved for Kubernetes or the Ownstack platform; choose another name", env)
		}
		if seenEnv[env] {
			add(path, "duplicate environment %q", env)
		}
		seenEnv[env] = true
	}

	for _, aspect := range sortedKeys(m.Spec.Components) {
		if _, ok := cat.Aspect(aspect); !ok {
			add("spec."+aspect, "unknown aspect")
		}
	}

	for _, aspect := range cat.Aspects {
		base := "spec." + aspect.ID
		c, ok := m.Component(aspect.ID)
		if !ok {
			if aspect.Required {
				add(base, "is required")
			}
			continue
		}
		p, ok := cat.Provider(aspect.ID, c.Provider)
		if !ok {
			add(base+".provider", "unknown %s provider %q", aspect.ID, c.Provider)
			continue
		}
		if !p.Available {
			add(base+".provider", "%s is planned but not implemented yet", p.Name)
			continue
		}
		for _, req := range p.Requires {
			if _, ok := m.Component(req); !ok {
				add(base, "%s needs a %s provider", p.Name, req)
			}
		}

		settings := m.Settings(cat, aspect.ID)
		for _, key := range sortedKeys(c.Settings) {
			param, ok := p.Param(key)
			if !ok {
				add(base+".settings."+key, "unknown setting for %s", p.Name)
			} else if param.IsSecret() {
				add(base+".settings."+key, "is a secret; reference it under secrets instead")
			}
		}
		for _, key := range sortedKeys(c.Secrets) {
			param, ok := p.Param(key)
			if !ok || !param.IsSecret() {
				add(base+".secrets."+key, "unknown secret for %s", p.Name)
				continue
			}
			if err := checkRef(c.Secrets[key]); err != nil {
				add(base+".secrets."+key, "%v", err)
			}
		}
		for _, param := range p.Params {
			if param.IsSecret() {
				continue
			}
			value, set := settings[param.Key]
			path := base + ".settings." + param.Key
			if !set || value == "" {
				if param.Required {
					add(path, "is required")
				}
				continue
			}
			if msg := checkKind(param, value, m.Environments()); msg != "" {
				add(path, "%s", msg)
			}
			if param.Key == catalog.ParamBranches {
				for _, msg := range checkBranches(value, defaultBranch(p, settings), m.Environments()) {
					add(path, "%s", msg)
				}
			}
		}
	}
	return issues
}

func checkRef(ref string) error {
	kind, value, ok := strings.Cut(ref, ":")
	switch {
	case !ok || value == "":
		return fmt.Errorf("must be env:NAME or file:/path")
	case kind == "env" && !envNameRe.MatchString(value):
		return fmt.Errorf("invalid environment variable name %q", value)
	case kind == "file" && !strings.HasPrefix(value, "/"):
		return fmt.Errorf("file references must be absolute paths")
	case kind != "env" && kind != "file":
		return fmt.Errorf("unsupported reference %q; use env: or file:", kind)
	}
	return nil
}

func checkKind(param catalog.Param, value string, environments []string) string {
	switch param.Kind {
	case catalog.KindHostname:
		if !hostnameRe.MatchString(value) {
			return "must be a hostname"
		}
	case catalog.KindIP:
		if ip := net.ParseIP(value); ip == nil || ip.To4() == nil {
			return "must be an IPv4 address"
		}
	case catalog.KindEmail:
		if _, err := mail.ParseAddress(value); err != nil || strings.Contains(value, "<") {
			return "must be an email address"
		}
	case catalog.KindInt:
		if _, err := strconv.Atoi(value); err != nil {
			return "must be a whole number"
		}
	case catalog.KindBool:
		if _, err := strconv.ParseBool(value); err != nil {
			return "must be true or false"
		}
	case catalog.KindEnum:
		if !slices.Contains(param.Enum, value) {
			return "must be one of " + strings.Join(param.Enum, ", ")
		}
	case catalog.KindEnvironment:
		if !slices.Contains(environments, value) {
			return "must be one of spec.environments (" + strings.Join(environments, ", ") + ")"
		}
	}
	if !param.MatchesPattern(value) {
		return param.PatternHelp
	}
	return ""
}

// branchRe is a branch name or a glob where '*' matches any run of characters,
// including '/'. '*' is its only special character: the workflow matches it as
// a bash case pattern, and Jenkins turns it into '.*' and quotes the rest.
var branchRe = regexp.MustCompile(`^[A-Za-z0-9*][A-Za-z0-9._/*-]*$`)

// defaultBranch is the branch a cicd provider deploys into its environment
// setting, the production environment: its deployBranch setting when it has
// one, else main.
func defaultBranch(p catalog.Provider, settings map[string]string) string {
	if _, ok := p.Param(catalog.ParamDeployBranch); ok && settings[catalog.ParamDeployBranch] != "" {
		return settings[catalog.ParamDeployBranch]
	}
	return catalog.DefaultBranch
}

// checkBranches checks a branches setting: <branch>=<environment> pairs
// separated by spaces, tabs, or newlines (where bash splits them), each
// branch listed once and never the default branch, which always maps to the
// environment setting. It returns one message per problem.
func checkBranches(value, defaultBranch string, environments []string) []string {
	var msgs []string
	seen := map[string]bool{}
	pairs := strings.FieldsFunc(value, func(r rune) bool { return r == ' ' || r == '\t' || r == '\n' })
	for _, pair := range pairs {
		branch, env, ok := strings.Cut(pair, "=")
		if !ok || strings.Contains(env, "=") {
			msgs = append(msgs, fmt.Sprintf("%q must be one <branch>=<environment> pair, such as develop=dev", pair))
			continue
		}
		switch {
		case !branchRe.MatchString(branch):
			msgs = append(msgs, fmt.Sprintf("branch %q must be a branch name or a glob of letters, digits, '.', '_', '-', '/' and '*'", branch))
		case branch == defaultBranch:
			msgs = append(msgs, fmt.Sprintf("branch %q always deploys into the production environment (the environment setting); remove it here", branch))
		case seen[branch]:
			msgs = append(msgs, fmt.Sprintf("branch %q is listed twice", branch))
		}
		seen[branch] = true
		if !slices.Contains(environments, env) {
			msgs = append(msgs, fmt.Sprintf("environment %q of %q must be one of spec.environments (%s)", env, pair, strings.Join(environments, ", ")))
		}
	}
	return msgs
}

// ── Writing ───────────────────────────────────────────────────────────────────

// Marshal writes the manifest with aspects in catalog order so the file reads
// top to bottom the way the stack is built.
func (m *Manifest) Marshal(cat *catalog.Catalog) ([]byte, error) {
	root := &yaml.Node{Kind: yaml.MappingNode}
	addScalar(root, "apiVersion", m.APIVersion)
	addScalar(root, "kind", m.Kind)
	meta := &yaml.Node{Kind: yaml.MappingNode}
	addScalar(meta, "name", m.Metadata.Name)
	addNode(root, "metadata", meta)

	spec := &yaml.Node{Kind: yaml.MappingNode}
	addScalar(spec, "domain", m.Spec.Domain)
	if m.Spec.AdminSubdomain != nil {
		addScalar(spec, "adminSubdomain", *m.Spec.AdminSubdomain)
	}
	if len(m.Spec.Environments) > 0 {
		envs := &yaml.Node{Kind: yaml.SequenceNode, Style: yaml.FlowStyle}
		for _, env := range m.Spec.Environments {
			envs.Content = append(envs.Content, scalar(env))
		}
		addNode(spec, "environments", envs)
	}

	order := cat.Order()
	for _, id := range sortedKeys(m.Spec.Components) {
		if _, known := cat.Aspect(id); !known {
			order = append(order, id)
		}
	}
	for _, aspect := range order {
		c, ok := m.Component(aspect)
		if !ok {
			continue
		}
		node := &yaml.Node{}
		if err := node.Encode(c); err != nil {
			return nil, err
		}
		addNode(spec, aspect, node)
	}
	addNode(root, "spec", spec)

	var buf bytes.Buffer
	buf.WriteString("# Ownstack stack definition. Reconciled on the node by `ownstackctl apply`.\n")
	buf.WriteString("# Secret values never live here: secrets are env:NAME or file:/path references,\n")
	buf.WriteString("# or omitted to use each provider's conventional environment variable.\n")
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(root); err != nil {
		return nil, err
	}
	return buf.Bytes(), enc.Close()
}

func scalar(v string) *yaml.Node { return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v} }

func addScalar(m *yaml.Node, key, value string) { addNode(m, key, scalar(value)) }

func addNode(m *yaml.Node, key string, value *yaml.Node) {
	m.Content = append(m.Content, scalar(key), value)
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
