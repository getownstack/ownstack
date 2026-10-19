package stack

import (
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
