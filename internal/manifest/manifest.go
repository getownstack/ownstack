// Package manifest reads, validates, and writes ownstack.yaml.
//
// ownstack.yaml describes a stack. It is flat and defaults-first: an aspect
// the file leaves out gets its default provider (the Starter preset) with
// default settings, so a complete Starter stack is four settings. Secret
// values never live in it; ownstackctl reads them on the node (see package
// secrets).
//
//	apiVersion: ownstack.io/v2
//	domain: example.com
//	compute: {host: 203.0.113.10}
//	ingress: {acmeEmail: ops@example.com}
//	vcs: {owner: acme}
package manifest

import (
	"bytes"
	"fmt"
	"net"
	"net/mail"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/getownstack/ownstack/internal/catalog"
)

const (
	APIVersion = "ownstack.io/v2"
	FileName   = "ownstack.yaml"

	DefaultAdminSubdomain = "admin"
)

// DefaultEnvironments apply when the manifest lists none.
var DefaultEnvironments = []string{"prod=main", "dev=develop", "qa=release/*"}

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

type Manifest struct {
	APIVersion string `yaml:"apiVersion"`
	Domain     string `yaml:"domain"`
	// AdminSubdomain places platform services at <svc>.<adminSubdomain>.<domain>.
	// nil means the default; an empty string places them directly under the domain.
	AdminSubdomain *string `yaml:"adminSubdomain,omitempty"`
	// Environments are <name>[=<branch glob>] items; see EnvironmentList.
	Environments []string `yaml:"environments,omitempty"`
	// Components are the aspects the file configures, by aspect id, and any
	// other top-level key (which Validate reports).
	Components map[string]*Component `yaml:",inline"`
	// Source is the file as it was read.
	Source []byte `yaml:"-"`
}

// Component is one aspect's entry: its provider, its settings as written,
// and Helm values merged over the provider's defaults.
type Component struct {
	Provider string
	Settings map[string]string
	Values   map[string]any
	// problems are keys whose value has the wrong shape, and why; "" is the
	// entry itself.
	problems map[string]string
}

// UnmarshalYAML reads a component leniently: wrongly shaped values become
// validation issues instead of parse errors.
func (c *Component) UnmarshalYAML(node *yaml.Node) error {
	*c = Component{Settings: map[string]string{}, problems: map[string]string{}}
	if node.Kind == yaml.ScalarNode && node.Tag == "!!null" {
		return nil
	}
	if node.Kind != yaml.MappingNode {
		c.problems[""] = "must be a mapping of settings, such as {provider: ...}"
		return nil
	}
	seen := map[string]bool{}
	for i := 0; i+1 < len(node.Content); i += 2 {
		key, value := node.Content[i].Value, node.Content[i+1]
		switch {
		case seen[key]:
			c.problems[key] = "is listed twice"
		case key == "values":
			if value.Tag == "!!null" {
				continue
			}
			if value.Kind != yaml.MappingNode || value.Decode(&c.Values) != nil {
				c.problems[key] = "must be a mapping of Helm values"
			}
		case value.Kind != yaml.ScalarNode:
			c.problems[key] = "must be a single value"
		case key == "provider":
			c.Provider = scalarValue(value)
		default:
			c.Settings[key] = scalarValue(value)
		}
		seen[key] = true
	}
	return nil
}

func scalarValue(node *yaml.Node) string {
	if node.Tag == "!!null" {
		return ""
	}
	return node.Value
}

// Parse reads a manifest in YAML (or JSON, which YAML includes).
func Parse(data []byte) (*Manifest, error) {
	var m Manifest
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	if m.Components == nil {
		m.Components = map[string]*Component{}
	}
	for id, c := range m.Components {
		if c == nil {
			// `dns:` with nothing under it.
			m.Components[id] = &Component{Settings: map[string]string{}}
		}
	}
	m.Source = data
	return &m, nil
}

func (m *Manifest) adminSubdomain() string {
	if m.AdminSubdomain == nil {
		return DefaultAdminSubdomain
	}
	return *m.AdminSubdomain
}

// AdminDomain is the parent domain of platform service hostnames.
func (m *Manifest) AdminDomain() string {
	if sub := m.adminSubdomain(); sub != "" {
		return sub + "." + m.Domain
	}
	return m.Domain
}

// Environment is one deploy environment: its name, which is also its
// namespace, and the branch glob that deploys into it (empty when no branch
// does). In a glob, * matches any characters, / included.
type Environment struct{ Name, Branch string }

// EnvironmentList parses the environments, whose items are <name> or
// <name>=<branch glob>. The first is production. A branch deploys into the
// first environment whose glob matches it.
func (m *Manifest) EnvironmentList() []Environment {
	items := m.environmentItems()
	out := make([]Environment, len(items))
	for i, item := range items {
		name, branch, _ := strings.Cut(item, "=")
		out[i] = Environment{Name: name, Branch: branch}
	}
	return out
}

func (m *Manifest) environmentItems() []string {
	if len(m.Environments) == 0 {
		return DefaultEnvironments
	}
	return m.Environments
}

// Provider is the provider of an aspect: the one the manifest names, else
// the aspect's default.
func (m *Manifest) Provider(cat *catalog.Catalog, aspect string) string {
	if c := m.Components[aspect]; c != nil && c.Provider != "" {
		return c.Provider
	}
	a, _ := cat.Aspect(aspect)
	return a.Default
}

// Settings returns an aspect's settings with the provider's defaults
// applied. An empty value counts as not set.
func (m *Manifest) Settings(cat *catalog.Catalog, aspect string) map[string]string {
	out := map[string]string{}
	if p, ok := cat.Provider(aspect, m.Provider(cat, aspect)); ok {
		for _, param := range p.Params {
			if param.Default != "" {
				out[param.Key] = param.Default
			}
		}
	}
	if c := m.Components[aspect]; c != nil {
		for k, v := range c.Settings {
			if v != "" {
				out[k] = v
			}
		}
	}
	return out
}

// Values returns an aspect's Helm value overrides.
func (m *Manifest) Values(aspect string) map[string]any {
	if c := m.Components[aspect]; c != nil {
		return c.Values
	}
	return nil
}

// ── Validation ────────────────────────────────────────────────────────────────

// Issue is a problem at a path such as domain, environments[1],
// compute.host, or cicd.provider. Its message stands on its own.
type Issue struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

func (i Issue) String() string { return i.Path + ": " + i.Message }

var (
	nameRe     = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	hostnameRe = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)
	// branchRe is a branch name or a glob where '*' matches any run of
	// characters, '/' included. '*' is its only special character: the
	// workflow matches it as a bash case pattern, and Jenkins turns it into
	// '.*' and quotes the rest.
	branchRe = regexp.MustCompile(`^[A-Za-z0-9*][A-Za-z0-9._/*-]*$`)
)

// Validate checks the manifest against the catalog.
func (m *Manifest) Validate(cat *catalog.Catalog) []Issue {
	var issues []Issue
	add := func(path, format string, args ...any) {
		issues = append(issues, Issue{Path: path, Message: fmt.Sprintf(format, args...)})
	}

	switch m.APIVersion {
	case APIVersion:
	case "ownstack.io/v1":
		// Nothing else of a v1 file means anything in v2.
		return []Issue{{Path: "apiVersion", Message: "ownstack.io/v1 manifests are no longer read; download the manifest again from the designer"}}
	default:
		add("apiVersion", "apiVersion must be %s", APIVersion)
	}
	if !hostnameRe.MatchString(m.Domain) {
		add("domain", "the domain must be a lowercase domain name such as example.com")
	}
	if sub := m.adminSubdomain(); sub != "" && !nameRe.MatchString(sub) {
		add("adminSubdomain", "adminSubdomain must be a lowercase DNS label, or empty for platform services directly under the domain")
	}
	seenEnv, seenBranch := map[string]bool{}, map[string]bool{}
	for i, item := range m.environmentItems() {
		path := fmt.Sprintf("environments[%d]", i)
		env, branch, mapped := strings.Cut(item, "=")
		switch {
		case !nameRe.MatchString(env):
			add(path, "environment %q must be <name> or <name>=<branch>, with a lowercase DNS label as the name", item)
		case ReservedEnvironment(env):
			add(path, "environment %q is reserved for Kubernetes or the Ownstack platform; choose another name", env)
		case seenEnv[env]:
			add(path, "environment %q is listed twice", env)
		}
		seenEnv[env] = true
		if !mapped {
			continue
		}
		switch {
		case !branchRe.MatchString(branch):
			add(path, "branch %q of environment %s must be a branch name or a glob of letters, digits, '.', '_', '-', '/' and '*'", branch, env)
		case seenBranch[branch]:
			add(path, "branch %q already deploys into an earlier environment", branch)
		}
		seenBranch[branch] = true
	}

	for _, key := range sortedKeys(m.Components) {
		if _, ok := cat.Aspect(key); !ok {
			add(key, "unknown key %q; the aspects are %s", key, strings.Join(aspectIDs(cat), ", "))
		}
	}
	for _, aspect := range cat.Aspects {
		issues = append(issues, m.validateAspect(cat, aspect)...)
	}
	return issues
}

func (m *Manifest) validateAspect(cat *catalog.Catalog, aspect catalog.Aspect) []Issue {
	var issues []Issue
	add := func(key, format string, args ...any) {
		path := aspect.ID
		if key != "" {
			path += "." + key
		}
		issues = append(issues, Issue{Path: path, Message: fmt.Sprintf(format, args...)})
	}
	c := m.Components[aspect.ID]
	if c == nil {
		c = &Component{}
	}
	if problem, ok := c.problems[""]; ok {
		add("", "%s %s", aspect.ID, problem)
		return issues
	}
	id := m.Provider(cat, aspect.ID)
	p, ok := cat.Provider(aspect.ID, id)
	if !ok {
		add("provider", "unknown %s provider %q; use one of %s", aspect.ID, id, strings.Join(cat.ProviderIDs(aspect.ID), ", "))
		return issues
	}
	for _, key := range sortedKeys(c.problems) {
		add(key, "%s %s %s", p.Name, key, c.problems[key])
	}
	for _, key := range sortedKeys(c.Settings) {
		param, ok := p.Param(key)
		switch {
		case !ok:
			add(key, "%s has no setting %q", p.Name, key)
		case param.IsSecret():
			add(key, "%s is a secret and never goes in ownstack.yaml; set %s on the server instead", p.Label(param), param.Env)
		}
	}
	settings := m.Settings(cat, aspect.ID)
	for _, param := range p.Params {
		if _, reported := c.problems[param.Key]; reported || param.IsSecret() {
			continue
		}
		value := settings[param.Key]
		if value == "" {
			if param.Required {
				add(param.Key, "%s is required", p.Label(param))
			}
			continue
		}
		if msg := checkKind(param, value); msg != "" {
			add(param.Key, "%s %s", p.Label(param), msg)
		}
	}
	return issues
}

func checkKind(param catalog.Param, value string) string {
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
	}
	if !param.MatchesPattern(value) {
		return param.PatternHelp
	}
	return ""
}

func aspectIDs(cat *catalog.Catalog) []string {
	ids := make([]string, len(cat.Aspects))
	for i, a := range cat.Aspects {
		ids[i] = a.ID
	}
	return ids
}

// ── Writing ───────────────────────────────────────────────────────────────────

// Marshal writes the manifest with only the values that differ from the
// defaults, aspects in catalog order and settings in the provider's order,
// and never a secret. Keys the catalog does not know follow, sorted.
func (m *Manifest) Marshal(cat *catalog.Catalog) ([]byte, error) {
	root := &yaml.Node{Kind: yaml.MappingNode}
	add(root, "apiVersion", str(m.APIVersion))
	add(root, "domain", str(m.Domain))
	if m.AdminSubdomain != nil && *m.AdminSubdomain != DefaultAdminSubdomain {
		add(root, "adminSubdomain", str(*m.AdminSubdomain))
	}
	if len(m.Environments) > 0 && !slices.Equal(m.Environments, DefaultEnvironments) {
		envs := &yaml.Node{Kind: yaml.SequenceNode, Style: yaml.FlowStyle}
		for _, env := range m.Environments {
			envs.Content = append(envs.Content, str(env))
		}
		add(root, "environments", envs)
	}
	order := aspectIDs(cat)
	for _, id := range sortedKeys(m.Components) {
		if !slices.Contains(order, id) {
			order = append(order, id)
		}
	}
	for _, aspect := range order {
		node, err := m.componentNode(cat, aspect)
		if err != nil {
			return nil, err
		}
		if node != nil {
			add(root, aspect, node)
		}
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(root); err != nil {
		return nil, err
	}
	return buf.Bytes(), enc.Close()
}

// componentNode is an aspect's entry without its defaults; nil when nothing
// differs from them.
func (m *Manifest) componentNode(cat *catalog.Catalog, aspect string) (*yaml.Node, error) {
	c := m.Components[aspect]
	if c == nil {
		return nil, nil
	}
	node := &yaml.Node{Kind: yaml.MappingNode, Style: yaml.FlowStyle}
	a, known := cat.Aspect(aspect)
	if c.Provider != "" && (!known || c.Provider != a.Default) {
		add(node, "provider", str(c.Provider))
	}
	p, _ := cat.Provider(aspect, m.Provider(cat, aspect))
	var keys []string
	for _, param := range p.Params {
		if _, ok := c.Settings[param.Key]; ok {
			keys = append(keys, param.Key)
		}
	}
	for _, key := range sortedKeys(c.Settings) {
		if !slices.Contains(keys, key) {
			keys = append(keys, key)
		}
	}
	for _, key := range keys {
		value := c.Settings[key]
		param, _ := p.Param(key)
		// A secret never goes in ownstack.yaml, even when the input had one
		// (Validate reports it).
		if value == "" || value == param.Default || param.IsSecret() {
			continue
		}
		scalar := str(value)
		switch {
		case param.Kind == catalog.KindBool && checkKind(param, value) == "":
			scalar.Tag = "!!bool"
		case param.Kind == catalog.KindInt && checkKind(param, value) == "":
			scalar.Tag = "!!int"
		}
		add(node, key, scalar)
	}
	if len(c.Values) > 0 {
		values := &yaml.Node{}
		if err := values.Encode(c.Values); err != nil {
			return nil, err
		}
		add(node, "values", values)
		node.Style = 0
	}
	if len(node.Content) == 0 {
		return nil, nil
	}
	return node, nil
}

func str(v string) *yaml.Node { return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v} }

func add(m *yaml.Node, key string, value *yaml.Node) {
	m.Content = append(m.Content, str(key), value)
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
