package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/getownstack/ownstack-cluster-template/internal/providers/providertest"
)

func TestUpsertFileSkipsIdenticalContent(t *testing.T) {
	stored := []byte("apiVersion: ownstack.io/v1\n")
	puts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer gh-token-value" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.Method {
		case http.MethodGet:
			json.NewEncoder(w).Encode(map[string]string{"sha": "abc", "content": base64.StdEncoding.EncodeToString(stored)})
		case http.MethodPut:
			puts++
			var body struct{ Content, SHA string }
			json.NewDecoder(r.Body).Decode(&body)
			if body.SHA != "abc" {
				t.Errorf("update must send the existing sha, got %q", body.SHA)
			}
			stored, _ = base64.StdEncoding.DecodeString(body.Content)
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	rt, _ := providertest.Runtime(t)
	p := Provider{APIURL: srv.URL}
	rt = rt.For("vcs")
	if err := p.Resolve(context.Background(), rt); err != nil {
		t.Fatal(err)
	}
	c := p.client(rt)
	changed, err := c.upsertFile(context.Background(), rt, "acme", "ownstack-cluster", "main", "ownstack.yaml", []byte("apiVersion: ownstack.io/v1\n"), "msg")
	if err != nil || changed || puts != 0 {
		t.Fatalf("identical content: changed=%v puts=%d err=%v", changed, puts, err)
	}
	changed, err = c.upsertFile(context.Background(), rt, "acme", "ownstack-cluster", "main", "ownstack.yaml", []byte("changed\n"), "msg")
	if err != nil || !changed || puts != 1 || string(stored) != "changed\n" {
		t.Fatalf("changed content: changed=%v puts=%d err=%v stored=%q", changed, puts, err, stored)
	}
}

func TestResolveOutputs(t *testing.T) {
	rt, _ := providertest.Runtime(t)
	if err := (Provider{}).Resolve(context.Background(), rt.For("vcs")); err != nil {
		t.Fatal(err)
	}
	vcs := rt.Out.VCS
	if vcs.ClusterRepoURL != "https://github.com/acme/ownstack-cluster.git" || vcs.Token != "gh-token-value" {
		t.Errorf("outputs = %+v", vcs)
	}
}
