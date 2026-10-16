// Package cloudflare implements the DNS contract with Cloudflare: A records
// for the domain and its wildcard, and DNS-01 certificate validation.
package cloudflare

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/getownstack/ownstack-cluster-template/internal/providers/dnscheck"
	"github.com/getownstack/ownstack-cluster-template/internal/stack"
)

type Provider struct {
	// BaseURL overrides the API endpoint in tests.
	BaseURL string
}

func (p Provider) Resolve(_ context.Context, rt *stack.Runtime) error {
	out := &stack.DNSOutput{Challenge: stack.ChallengeDNS, ACMEProvider: "cloudflare", ACMEEnv: map[string]string{}}
	rt.Out.DNS = out
	token, err := rt.Secret("apiToken")
	if err != nil {
		return err
	}
	out.ACMEEnv["CF_DNS_API_TOKEN"] = token
	return nil
}

func (p Provider) Plan(ctx context.Context, rt *stack.Runtime) []stack.Step {
	domain, ip := rt.Manifest.Spec.Domain, rt.Out.Compute.PublicIP
	if dnscheck.AllMatch(ctx, domain, ip) {
		return []stack.Step{{Action: "verify", Target: "DNS records", Detail: fmt.Sprintf("%s and *.%s already resolve to %s", domain, domain, ip)}}
	}
	return []stack.Step{{Action: "configure", Target: "DNS records", Detail: fmt.Sprintf("A %s and *.%s -> %s (DNS only, not proxied)", domain, domain, ip)}}
}

func (p Provider) Apply(ctx context.Context, rt *stack.Runtime) error {
	api := p.client(rt)
	domain, ip := rt.Manifest.Spec.Domain, rt.Out.Compute.PublicIP

	var zoneID string
	err := stack.Retry(ctx, rt.Console, "find Cloudflare zone", 5, 10*time.Second, func() error {
		var err error
		zoneID, err = api.findZone(ctx, rt.Setting("zone"), domain)
		return err
	})
	if err != nil {
		return err
	}

	for _, name := range []string{domain, "*." + domain} {
		rt.Logf("DNS: A %s -> %s", name, ip)
		if err := stack.Retry(ctx, rt.Console, "upsert "+name, 5, 10*time.Second, func() error {
			return api.upsertA(ctx, zoneID, name, ip)
		}); err != nil {
			return err
		}
	}
	return dnscheck.WaitForRecords(ctx, rt, 10*time.Minute)
}

func (p Provider) Status(ctx context.Context, rt *stack.Runtime) []stack.Check {
	return dnscheck.Checks(ctx, rt)
}

// ── API client ───────────────────────────────────────────────────────────────

type client struct {
	base  string
	token string
	http  *http.Client
}

func (p Provider) client(rt *stack.Runtime) *client {
	base := p.BaseURL
	if base == "" {
		base = "https://api.cloudflare.com/client/v4"
	}
	return &client{base: base, token: rt.Out.DNS.ACMEEnv["CF_DNS_API_TOKEN"], http: rt.HTTP}
}

type record struct {
	ID      string `json:"id,omitempty"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	TTL     int    `json:"ttl"`
	Proxied bool   `json:"proxied"`
}

func (c *client) do(ctx context.Context, method, path string, body any, out any) error {
	var reader *bytes.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	} else {
		reader = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	res, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	var envelope struct {
		Success bool                       `json:"success"`
		Errors  []struct{ Message string } `json:"errors"`
		Result  json.RawMessage            `json:"result"`
	}
	if err := json.NewDecoder(res.Body).Decode(&envelope); err != nil {
		return fmt.Errorf("cloudflare %s %s: HTTP %d", method, path, res.StatusCode)
	}
	if !envelope.Success {
		var msgs []string
		for _, e := range envelope.Errors {
			msgs = append(msgs, e.Message)
		}
		return fmt.Errorf("cloudflare %s: HTTP %d: %s", method, res.StatusCode, strings.Join(msgs, "; "))
	}
	if out != nil {
		return json.Unmarshal(envelope.Result, out)
	}
	return nil
}

// findZone returns the zone for the explicit zone name, or the closest
// enclosing zone of the domain.
func (c *client) findZone(ctx context.Context, explicit, domain string) (string, error) {
	candidates := []string{explicit}
	if explicit == "" {
		candidates = nil
		labels := strings.Split(domain, ".")
		for i := 0; i < len(labels)-1; i++ {
			candidates = append(candidates, strings.Join(labels[i:], "."))
		}
	}
	for _, name := range candidates {
		var zones []struct{ ID string }
		if err := c.do(ctx, http.MethodGet, "/zones?name="+url.QueryEscape(name)+"&per_page=1", nil, &zones); err != nil {
			return "", err
		}
		if len(zones) > 0 {
			return zones[0].ID, nil
		}
	}
	return "", fmt.Errorf("no Cloudflare zone found for %s; check the token can read the zone", domain)
}

// upsertA makes name resolve only to ip: it keeps or creates one A record,
// and removes other A, AAAA, and CNAME records for the same name.
func (c *client) upsertA(ctx context.Context, zoneID, name, ip string) error {
	var existing []record
	path := fmt.Sprintf("/zones/%s/dns_records?name=%s&per_page=100", zoneID, url.QueryEscape(name))
	if err := c.do(ctx, http.MethodGet, path, nil, &existing); err != nil {
		return err
	}
	var keep string
	for _, r := range existing {
		if r.Type == "A" && keep == "" {
			keep = r.ID
			continue
		}
		if r.Type == "A" || r.Type == "AAAA" || r.Type == "CNAME" {
			if err := c.do(ctx, http.MethodDelete, fmt.Sprintf("/zones/%s/dns_records/%s", zoneID, r.ID), nil, nil); err != nil {
				return err
			}
		}
	}
	want := record{Type: "A", Name: name, Content: ip, TTL: 1, Proxied: false}
	if keep != "" {
		return c.do(ctx, http.MethodPut, fmt.Sprintf("/zones/%s/dns_records/%s", zoneID, keep), want, nil)
	}
	return c.do(ctx, http.MethodPost, fmt.Sprintf("/zones/%s/dns_records", zoneID), want, nil)
}
