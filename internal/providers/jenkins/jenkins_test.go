package jenkins

import (
	"context"
	"encoding/xml"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/getownstack/ownstack-cluster-template/internal/providers/providertest"
	"github.com/getownstack/ownstack-cluster-template/internal/providers/slack"
	"github.com/getownstack/ownstack-cluster-template/internal/stack"
)

func TestValuesRenderWithAlerts(t *testing.T) {
	rt, _ := providertest.Runtime(t)
	rt.Out.VCS = &stack.VCSOutput{Kind: "github", Owner: "acme", ClusterRepoURL: "https://github.com/acme/ownstack-cluster.git"}
	rt.Out.Images = &stack.ImagesOutput{Host: "harbor.admin.example.com", Project: "product", PullSecretName: "harbor-robot"}
	if err := (slack.Provider{}).Resolve(context.Background(), rt.For("alerts")); err != nil {
		t.Fatal(err)
	}
	jrt := rt.For("cicd")
	values, err := stack.Render(valuesTmpl, valuesData(jrt, "pw"))
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Controller struct {
			JCasC struct {
				ConfigScripts map[string]string `yaml:"configScripts"`
			} `yaml:"JCasC"`
			Ingress struct {
				HostName string `yaml:"hostName"`
			}
		}
	}
	if err := yaml.Unmarshal([]byte(values), &parsed); err != nil {
		t.Fatalf("invalid values YAML: %v\n%s", err, values)
	}
	if parsed.Controller.Ingress.HostName != "jenkins.admin.example.com" {
		t.Errorf("host = %q", parsed.Controller.Ingress.HostName)
	}
	casc := parsed.Controller.JCasC.ConfigScripts["main"]
	var cascDoc map[string]any
	if err := yaml.Unmarshal([]byte(casc), &cascDoc); err != nil {
		t.Fatalf("invalid JCasC YAML: %v\n%s", err, casc)
	}
	for _, want := range []string{"IMAGE_REGISTRY", "harbor.admin.example.com", "OWNSTACK_SLACK_CHANNEL", "slackNotifier", "ownstack-cluster.git"} {
		if !strings.Contains(casc, want) {
			t.Errorf("JCasC missing %q", want)
		}
	}
	if strings.Contains(values, "xoxb-slack-token") {
		t.Error("Slack token must be a credential, not a value")
	}
}

func TestOrgFolderXML(t *testing.T) {
	out, err := stack.Render(orgFolderTmpl, map[string]string{
		"Owner": "acme", "CredentialID": "github-pat", "RepoFilter": "app-* <&>", "JenkinsfilePath": "infrastructure/Jenkinsfile",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Jenkins writes XML 1.1, which encoding/xml refuses; the body is 1.0-compatible.
	body := out[strings.Index(out, "?>")+2:]
	dec := xml.NewDecoder(strings.NewReader(body))
	for {
		if _, err := dec.Token(); err != nil {
			if err.Error() == "EOF" {
				break
			}
			t.Fatalf("invalid XML: %v", err)
		}
	}
	if !strings.Contains(out, "<repoOwner>acme</repoOwner>") || !strings.Contains(out, "app-* &lt;&amp;&gt;") {
		t.Errorf("unexpected XML:\n%s", out)
	}
}
