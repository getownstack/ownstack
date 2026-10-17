package cloudflare

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/getownstack/ownstack-cluster-template/internal/providers/providertest"
)

// fakeCloudflare keeps DNS records for one zone, example.com.
type fakeCloudflare struct {
	mu      sync.Mutex
	records map[string]record
	nextID  int
	calls   []string
}

func (f *fakeCloudflare) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	if r.Header.Get("Authorization") != "Bearer cf-token-value" {
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(map[string]any{"success": false, "errors": []map[string]string{{"message": "bad token"}}})
		return
	}
	var result any
	switch {
	case r.URL.Path == "/zones":
		zones := []map[string]string{}
		if r.URL.Query().Get("name") == "example.com" {
			zones = append(zones, map[string]string{"id": "zone1"})
		}
		result = zones
	case r.URL.Path == "/zones/zone1/dns_records" && r.Method == http.MethodGet:
		var matches []record
		for _, rec := range f.records {
			if rec.Name == r.URL.Query().Get("name") {
				matches = append(matches, rec)
			}
		}
		result = matches
	case r.URL.Path == "/zones/zone1/dns_records" && r.Method == http.MethodPost:
		var rec record
		json.NewDecoder(r.Body).Decode(&rec)
		f.nextID++
		rec.ID = "new" + string(rune('0'+f.nextID))
		f.records[rec.ID] = rec
		result = rec
	case strings.HasPrefix(r.URL.Path, "/zones/zone1/dns_records/"):
		id := strings.TrimPrefix(r.URL.Path, "/zones/zone1/dns_records/")
		if r.Method == http.MethodDelete {
			delete(f.records, id)
		} else {
			var rec record
			json.NewDecoder(r.Body).Decode(&rec)
			rec.ID = id
			f.records[id] = rec
		}
		result = map[string]string{"id": id}
	default:
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]any{"success": false})
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"success": true, "result": result})
}

func TestUpsertRecords(t *testing.T) {
	fake := &fakeCloudflare{records: map[string]record{
		"a1":    {ID: "a1", Type: "A", Name: "example.com", Content: "198.51.100.1", Proxied: true},
		"a2":    {ID: "a2", Type: "A", Name: "example.com", Content: "198.51.100.2"},
		"cname": {ID: "cname", Type: "CNAME", Name: "*.example.com", Content: "elsewhere.net"},
		"mx":    {ID: "mx", Type: "MX", Name: "example.com", Content: "mail.example.com"},
	}}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	rt, _ := providertest.Runtime(t)
	rt = rt.For("dns")
	p := Provider{BaseURL: srv.URL}
	api := p.client(rt)
	ctx := context.Background()

	zone, err := api.findZone(ctx, "", "sub.example.com")
	if err != nil || zone != "zone1" {
		t.Fatalf("zone = %q, %v", zone, err)
	}
	for _, name := range []string{"example.com", "*.example.com"} {
		if err := api.upsertA(ctx, zone, name, "203.0.113.10"); err != nil {
			t.Fatal(err)
		}
	}

	byName := map[string][]record{}
	for _, rec := range fake.records {
		byName[rec.Name] = append(byName[rec.Name], rec)
	}
	for _, name := range []string{"example.com", "*.example.com"} {
		var as []record
		for _, rec := range byName[name] {
			if rec.Type == "A" || rec.Type == "CNAME" || rec.Type == "AAAA" {
				as = append(as, rec)
			}
		}
		if len(as) != 1 || as[0].Type != "A" || as[0].Content != "203.0.113.10" || as[0].Proxied {
			t.Errorf("%s records = %+v", name, as)
		}
	}
	if _, ok := fake.records["mx"]; !ok {
		t.Error("unrelated MX record was removed")
	}
	if _, ok := fake.records["a1"]; !ok {
		t.Error("existing A record should be updated in place, not replaced")
	}
}

func TestResolveUsesSecret(t *testing.T) {
	rt, _ := providertest.Runtime(t)
	rt.Out.DNS = nil
	if err := (Provider{}).Resolve(context.Background(), rt.For("dns")); err != nil {
		t.Fatal(err)
	}
	if rt.Out.DNS.ACMEEnv["CF_DNS_API_TOKEN"] != "cf-token-value" {
		t.Errorf("ACME env = %v", rt.Out.DNS.ACMEEnv)
	}
}
