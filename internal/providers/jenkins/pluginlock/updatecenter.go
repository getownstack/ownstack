package pluginlock

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
)

// UpdateCenterURL is the update center tailored to a Jenkins core: it
// redirects to the newest plugin release of each plugin that the core's line
// (dynamic-stable-<core>) supports.
func UpdateCenterURL(core string) string {
	return "https://updates.jenkins.io/update-center.actual.json?version=" + url.QueryEscape(core)
}

// UpdateCenter is the part of an update-center.json that lock resolution reads.
type UpdateCenter struct {
	Core         Core                   `json:"core"`
	Plugins      map[string]Plugin      `json:"plugins"`
	Warnings     []Warning              `json:"warnings"`
	Deprecations map[string]Deprecation `json:"deprecations"`
}

type Core struct {
	Version string `json:"version"`
}

type Plugin struct {
	Name         string       `json:"name"`
	Version      string       `json:"version"`
	RequiredCore string       `json:"requiredCore"`
	Dependencies []Dependency `json:"dependencies"`
}

// Dependency is a plugin's dependency on at least Version of another plugin.
type Dependency struct {
	Name     string `json:"name"`
	Version  string `json:"version"`
	Optional bool   `json:"optional"`
}

// Warning is a published security warning (type "plugin" or "core").
type Warning struct {
	ID       string           `json:"id"`
	Message  string           `json:"message"`
	Name     string           `json:"name"`
	Type     string           `json:"type"`
	URL      string           `json:"url"`
	Versions []WarningVersion `json:"versions"`
}

// WarningVersion is one affected version range. Pattern is a Java regular
// expression the whole version must match.
type WarningVersion struct {
	FirstVersion string `json:"firstVersion"`
	LastVersion  string `json:"lastVersion"`
	Pattern      string `json:"pattern"`
}

type Deprecation struct {
	URL string `json:"url"`
}

// ParseUpdateCenter reads update-center.actual.json, or update-center.json
// with its JSONP wrapper.
func ParseUpdateCenter(data []byte) (*UpdateCenter, error) {
	data = bytes.TrimSpace(data)
	if i := bytes.IndexByte(data, '{'); i > 0 && bytes.HasSuffix(data, []byte(");")) {
		data = data[i : len(data)-2]
	}
	var uc UpdateCenter
	if err := json.Unmarshal(data, &uc); err != nil {
		return nil, fmt.Errorf("update center: %w", err)
	}
	if uc.Core.Version == "" || len(uc.Plugins) == 0 {
		return nil, fmt.Errorf("update center: no core version or plugins")
	}
	for name, p := range uc.Plugins {
		if p.Name == "" {
			p.Name = name
			uc.Plugins[name] = p
		}
	}
	return &uc, nil
}

// Matches reports whether version is in the warning's affected range. The
// pattern is matched against the whole version, as Jenkins does. A pattern Go
// cannot compile falls back to firstVersion..lastVersion, so a warning is
// never silently skipped.
func (v WarningVersion) Matches(version string) bool {
	if v.Pattern != "" {
		if re, err := regexp.Compile(`^(?:` + v.Pattern + `)$`); err == nil {
			return re.MatchString(version)
		}
	}
	if v.LastVersion == "" {
		return v.Pattern != "" // unreadable and unbounded: assume affected
	}
	if Compare(version, v.LastVersion) > 0 {
		return false
	}
	return v.FirstVersion == "" || Compare(version, v.FirstVersion) >= 0
}

// Affects reports whether the warning applies to the named plugin (or core,
// with name "core") at version.
func (w Warning) Affects(name, version string) bool {
	if w.Name != name {
		return false
	}
	for _, v := range w.Versions {
		if v.Matches(version) {
			return true
		}
	}
	return false
}
