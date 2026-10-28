package cloudflare

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/getownstack/ownstack/internal/providers/providertest"
	"github.com/getownstack/ownstack/internal/stack"
)

// fakeCloudflare keeps DNS records for one zone, example.com.
type fakeCloudflare struct {
	mu      sync.Mutex
	records map[string]record
	nextID  int
	calls   []string
}

// sorted lists the records by id, so responses do not depend on map order.
func (f *fakeCloudflare) sorted() []record {
	var out []record
	for _, rec := range f.records {
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
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
		switch r.Method {
		case http.MethodDelete:
			delete(f.records, id)
		case http.MethodPatch:
			// PATCH changes only the fields it sends.
			rec := f.records[id]
			json.NewDecoder(r.Body).Decode(&rec)
			rec.ID = id
			f.records[id] = rec
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
			json.NewEncoder(w).Encode(map[string]any{"success": false})
			return
		}
		result = map[string]string{"id": id}
	default:
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]any{"success": false})
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"success": true, "result": result})
}

// newFake serves records for zone example.com and returns a provider that
// uses it, with a runtime scoped to dns.
func newFake(t *testing.T, records ...record) (*fakeCloudflare, Provider, *stack.Runtime) {
	t.Helper()
	fake := &fakeCloudflare{records: map[string]record{}}
	for _, r := range records {
		fake.records[r.ID] = r
	}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	rt, _ := providertest.Runtime(t)
	return fake, Provider{BaseURL: srv.URL}, rt.For("dns")
}

// writes lists the calls that changed records.
func (f *fakeCloudflare) writes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		if !strings.HasPrefix(c, http.MethodGet) {
			out = append(out, c)
		}
	}
	return out
}

// addresses returns the A, AAAA, and CNAME records for name.
func (f *fakeCloudflare) addresses(name string) []record {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []record
	for _, rec := range f.sorted() {
		if rec.Name == name && (rec.Type == "A" || rec.Type == "AAAA" || rec.Type == "CNAME") {
			out = append(out, rec)
		}
	}
	return out
}

// foreignRecords is a zone whose apex and wildcard someone else uses.
var foreignRecords = []record{
	{ID: "a1", Type: "A", Name: "example.com", Content: "198.51.100.1", Proxied: true},
	{ID: "a2", Type: "A", Name: "example.com", Content: "198.51.100.2"},
	{ID: "cname", Type: "CNAME", Name: "*.example.com", Content: "elsewhere.net"},
	{ID: "mx", Type: "MX", Name: "example.com", Content: "mail.example.com"},
}

func TestUpsertRefusesRecordsItDidNotCreate(t *testing.T) {
	fake, p, rt := newFake(t, foreignRecords...)
	api := p.client(rt)
	ctx := context.Background()
	zone, err := api.findZone(ctx, "", "sub.example.com")
	if err != nil || zone != "zone1" {
		t.Fatalf("zone = %q, %v", zone, err)
	}
	for _, name := range []string{"example.com", "*.example.com"} {
		_, err := api.upsertA(ctx, zone, name, "203.0.113.10", false)
		if err == nil || !strings.Contains(err.Error(), "spec.dns.settings.replaceRecords") {
			t.Errorf("%s: err = %v, want a refusal naming the setting", name, err)
		}
	}
	if w := fake.writes(); len(w) != 0 {
		t.Errorf("changed records without replaceRecords: %v", w)
	}
}

func TestUpsertReplacesWhenAllowed(t *testing.T) {
	fake, p, rt := newFake(t, foreignRecords...)
	api := p.client(rt)
	ctx := context.Background()
	for _, name := range []string{"example.com", "*.example.com"} {
		replaced, err := api.upsertA(ctx, "zone1", name, "203.0.113.10", true)
		if err != nil {
			t.Fatal(err)
		}
		if len(replaced) == 0 {
			t.Errorf("%s: replaced records not reported", name)
		}
	}
	for _, name := range []string{"example.com", "*.example.com"} {
		as := fake.addresses(name)
		if len(as) != 1 || as[0].Type != "A" || as[0].Content != "203.0.113.10" || as[0].Proxied || as[0].Comment != recordComment {
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

func TestUpsertAdoptsAndKeepsOwnedRecords(t *testing.T) {
	fake, p, rt := newFake(t,
		// Written by an earlier ownstackctl: no comment, already the server.
		record{ID: "apex", Type: "A", Name: "example.com", Content: "203.0.113.10", TTL: 1},
		// The user's own note on a record that points at the server.
		record{ID: "wild", Type: "A", Name: "*.example.com", Content: "203.0.113.10", TTL: 1, Comment: "platform"},
	)
	api := p.client(rt)
	ctx := context.Background()
	for _, name := range []string{"example.com", "*.example.com"} {
		if _, err := api.upsertA(ctx, "zone1", name, "203.0.113.10", false); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if got := fake.records["apex"].Comment; got != recordComment {
		t.Errorf("apex comment = %q", got)
	}
	if got := fake.records["wild"].Comment; got != "platform; "+recordComment {
		t.Errorf("wildcard comment = %q, want the user's text kept", got)
	}
	writes := len(fake.writes())
	for _, name := range []string{"example.com", "*.example.com"} {
		if _, err := api.upsertA(ctx, "zone1", name, "203.0.113.10", false); err != nil {
			t.Fatal(err)
		}
	}
	if w := fake.writes(); len(w) != writes {
		t.Errorf("a rerun changed records: %v", w[writes:])
	}

	// The server moved: records ownstackctl marked follow it without the flag.
	for _, name := range []string{"example.com", "*.example.com"} {
		if _, err := api.upsertA(ctx, "zone1", name, "192.0.2.20", false); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if fake.records["apex"].Content != "192.0.2.20" || fake.records["wild"].Content != "192.0.2.20" {
		t.Errorf("records = %+v", fake.records)
	}
}

func TestApplyRefusesBeforeChangingAnything(t *testing.T) {
	// The apex is ownstackctl's; only the wildcard is someone else's. The
	// apex must not be touched either.
	fake, p, rt := newFake(t,
		record{ID: "apex", Type: "A", Name: "example.com", Content: "198.51.100.9", Comment: recordComment},
		record{ID: "cname", Type: "CNAME", Name: "*.example.com", Content: "elsewhere.net"},
	)
	err := p.Apply(context.Background(), rt)
	if err == nil || !strings.Contains(err.Error(), "*.example.com: CNAME elsewhere.net") || !strings.Contains(err.Error(), "spec.dns.settings.replaceRecords: true") {
		t.Fatalf("err = %v", err)
	}
	if w := fake.writes(); len(w) != 0 {
		t.Errorf("changed records: %v", w)
	}
}

func TestPlanFromRecords(t *testing.T) {
	fake, p, rt := newFake(t,
		record{ID: "apex", Type: "A", Name: "example.com", Content: "203.0.113.10", Comment: recordComment},
		record{ID: "cname", Type: "CNAME", Name: "*.example.com", Content: "elsewhere.net"},
	)
	steps := p.Plan(context.Background(), rt)
	if len(steps) != 2 || steps[0].Action != "verify" || steps[1].Action != "replace" ||
		!strings.Contains(steps[1].Detail, "CNAME elsewhere.net") || !strings.Contains(steps[1].Detail, "replaceRecords") {
		t.Errorf("steps = %+v", steps)
	}
	if w := fake.writes(); len(w) != 0 {
		t.Errorf("plan changed records: %v", w)
	}
}

func TestPlanRecordsIsOrderIndependent(t *testing.T) {
	recs := []record{
		{ID: "b", Type: "A", Name: "example.com", Content: "198.51.100.2"},
		{ID: "a", Type: "A", Name: "example.com", Content: "198.51.100.1"},
		{ID: "c", Type: "AAAA", Name: "example.com", Content: "2001:db8::1"},
		{ID: "d", Type: "A", Name: "example.com", Content: "198.51.100.3", Comment: "x; " + recordComment},
		{ID: "e", Type: "TXT", Name: "example.com", Content: "v=spf1"},
	}
	for i := 0; i < len(recs); i++ {
		rotated := append(append([]record(nil), recs[i:]...), recs[:i]...)
		plan := planRecords(rotated, "203.0.113.10")
		// The owned record is kept, the others are someone else's.
		if plan.keep == nil || plan.keep.ID != "d" || len(plan.remove) != 3 || len(plan.foreign) != 3 {
			t.Fatalf("rotation %d: keep=%+v remove=%+v foreign=%+v", i, plan.keep, plan.remove, plan.foreign)
		}
		if plan.want.Comment != "x; "+recordComment {
			t.Errorf("rotation %d: comment = %q", i, plan.want.Comment)
		}
	}
	if plan := planRecords(recs[:2], "203.0.113.10"); plan.keep.ID != "a" {
		t.Errorf("keep = %s, want the lowest id among equals", plan.keep.ID)
	}
}

func TestOwnedComment(t *testing.T) {
	long := strings.Repeat("x", maxComment-5)
	for in, want := range map[string]string{
		"":                      recordComment,
		recordComment:           recordComment,
		"web; " + recordComment: "web; " + recordComment,
		"web":                   "web; " + recordComment,
		long:                    long,
	} {
		if got := ownedComment(in); got != want {
			t.Errorf("ownedComment(%q) = %q, want %q", in, got, want)
		}
		if got := ownedComment(in); len(got) > maxComment {
			t.Errorf("comment longer than %d: %q", maxComment, got)
		}
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
