package stack

import (
	"encoding/base64"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestPullSecretMismatch(t *testing.T) {
	const server, user, pass = "harbor.admin.example.com", "robot$ownstack-ci", "s3cr3t-value"
	current, err := DockerConfigJSON(server, user, pass)
	if err != nil {
		t.Fatal(err)
	}
	hub, _ := DockerConfigJSON("https://index.docker.io/v1/", "acme", pass)
	for _, tc := range []struct {
		name, raw, server, user, pass, want string
	}{
		{"match", current, server, user, pass, ""},
		{"rotated password", current, server, user, "new-value", "different password"},
		{"new robot", current, server, "robot$other", pass, "different username"},
		{"password unknown here", current, server, user, "", ""},
		{"other registry", current, "zot.admin.example.com", user, pass, "no entry for zot.admin.example.com"},
		{"docker hub key", hub, "https://index.docker.io/v1/", "acme", pass, ""},
		{"auth only", `{"auths":{"r.example.com":{"auth":"dTpw"}}}`, "r.example.com", "u", "p", ""},
		{"garbage", "not json", server, user, pass, "unreadable"},
	} {
		got := PullSecretMismatch(tc.raw, tc.server, tc.user, tc.pass)
		if got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
		if strings.Contains(got, pass) || strings.Contains(got, "new-value") {
			t.Errorf("%s: reason contains a password: %q", tc.name, got)
		}
	}
}

func TestDockerConfigJSON(t *testing.T) {
	for _, tc := range []struct{ server, user, pass string }{
		{"harbor.admin.example.com", "robot$ownstack-ci", `p"a:ss\w`},
		{"https://index.docker.io/v1/", "acme", "token"},
	} {
		raw, err := DockerConfigJSON(tc.server, tc.user, tc.pass)
		if err != nil {
			t.Fatal(err)
		}
		var config struct {
			Auths map[string]struct{ Username, Password, Auth string } `json:"auths"`
		}
		if err := json.Unmarshal([]byte(raw), &config); err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		entry, ok := config.Auths[tc.server]
		if !ok || len(config.Auths) != 1 {
			t.Fatalf("auths = %v, want only %s", config.Auths, tc.server)
		}
		decoded, _ := base64.StdEncoding.DecodeString(entry.Auth)
		if entry.Username != tc.user || entry.Password != tc.pass || string(decoded) != tc.user+":"+tc.pass {
			t.Errorf("%s: entry = %+v, auth = %q", tc.server, entry, decoded)
		}
	}
}

func TestImagePullSecretsPatch(t *testing.T) {
	for _, tc := range []struct {
		name, sa, want string
		changed        bool
	}{
		{"fresh account", `{"metadata":{"name":"default","resourceVersion":"41"}}`,
			`{"imagePullSecrets":[{"name":"harbor-robot"}],"metadata":{"resourceVersion":"41"}}`, true},
		{"keeps existing entries", `{"metadata":{"resourceVersion":"7"},"imagePullSecrets":[{"name":"other"}]}`,
			`{"imagePullSecrets":[{"name":"other"},{"name":"harbor-robot"}],"metadata":{"resourceVersion":"7"}}`, true},
		{"already listed", `{"metadata":{"resourceVersion":"7"},"imagePullSecrets":[{"name":"other"},{"name":"harbor-robot"}]}`, "", false},
	} {
		patch, changed, err := imagePullSecretsPatch(tc.sa, "harbor-robot")
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if changed != tc.changed {
			t.Errorf("%s: changed = %v", tc.name, changed)
		}
		if tc.want != "" {
			var got, want any
			json.Unmarshal([]byte(patch), &got)
			json.Unmarshal([]byte(tc.want), &want)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("%s: patch = %s, want %s", tc.name, patch, tc.want)
			}
		}
	}
	if _, _, err := imagePullSecretsPatch("not json", "x"); err == nil {
		t.Error("unreadable service account accepted")
	}
}
