// Package cloudflare implements the DNS contract with Cloudflare: A records
// for the domain and its wildcard, and DNS-01 certificate validation.
//
// Records ownstackctl writes carry a comment (recordComment). A record is
// ownstackctl's when its comment says so or when it is an A record that
// already points at the server; any other A, AAAA, or CNAME record for the
// two names belongs to someone else and is only replaced when the
// replaceRecords setting allows it.
package cloudflare

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
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

// replaceSetting is the manifest path of the setting that allows replacing
// records ownstackctl did not create.
const replaceSetting = "spec.dns.settings.replaceRecords"

func (p Provider) Plan(ctx context.Context, rt *stack.Runtime) []stack.Step {
	domain, ip := rt.Manifest.Spec.Domain, rt.Out.Compute.PublicIP
	// The records themselves tell what apply would change; public DNS shows
	// Cloudflare's addresses for proxied records. Without a token (plan
	// before the secrets exist) public DNS is the fallback.
	if rt.Out.DNS != nil && rt.Out.DNS.ACMEEnv["CF_DNS_API_TOKEN"] != "" {
		if steps, err := p.planFromRecords(ctx, rt); err == nil {
			return steps
		}
	}
	if dnscheck.AllMatch(ctx, domain, ip) {
		return []stack.Step{{Action: "verify", Target: "DNS records", Detail: fmt.Sprintf("%s and *.%s already resolve to %s", domain, domain, ip)}}
	}
	return []stack.Step{{Action: "configure", Target: "DNS records", Detail: fmt.Sprintf("A %s and *.%s -> %s (DNS only, not proxied)", domain, domain, ip)}}
}

// planFromRecords describes what apply does to each name's records.
func (p Provider) planFromRecords(ctx context.Context, rt *stack.Runtime) ([]stack.Step, error) {
	api := p.client(rt)
	domain, ip := rt.Manifest.Spec.Domain, rt.Out.Compute.PublicIP
	zoneID, err := api.findZone(ctx, rt.Setting("zone"), domain)
	if err != nil {
		return nil, err
	}
	var steps []stack.Step
	for _, name := range recordNames(domain) {
		existing, err := api.records(ctx, zoneID, name)
		if err != nil {
			return nil, err
		}
		plan := planRecords(existing, ip)
		target := "A " + name
		switch {
		case len(plan.foreign) > 0 && rt.SettingBool("replaceRecords"):
			steps = append(steps, stack.Step{Action: "replace", Target: target, Detail: fmt.Sprintf("currently %s -> %s", describe(plan.foreign), ip)})
		case len(plan.foreign) > 0:
			steps = append(steps, stack.Step{Action: "replace", Target: target, Detail: fmt.Sprintf("currently %s, not created by ownstackctl; apply stops unless %s is true", describe(plan.foreign), replaceSetting)})
		case plan.unchanged():
			steps = append(steps, stack.Step{Action: "verify", Target: target, Detail: ip})
		case plan.keep != nil && plan.keep.Content != ip:
			steps = append(steps, stack.Step{Action: "configure", Target: target, Detail: fmt.Sprintf("currently %s -> %s (DNS only, not proxied)", plan.keep.Content, ip)})
		default:
			steps = append(steps, stack.Step{Action: "configure", Target: target, Detail: ip + " (DNS only, not proxied)"})
		}
	}
	return steps, nil
}

func (p Provider) Apply(ctx context.Context, rt *stack.Runtime) error {
	api := p.client(rt)
	domain, ip := rt.Manifest.Spec.Domain, rt.Out.Compute.PublicIP
	replace := rt.SettingBool("replaceRecords")

	var zoneID string
	err := stack.Retry(ctx, rt.Console, "find Cloudflare zone", 5, 10*time.Second, func() error {
		var err error
		zoneID, err = api.findZone(ctx, rt.Setting("zone"), domain)
		return err
	})
	if err != nil {
		return err
	}

	// Check both names before changing either, so a refusal leaves the zone
	// as it was.
	if !replace {
		var foreign []string
		for _, name := range recordNames(domain) {
			var existing []record
			if err := stack.Retry(ctx, rt.Console, "read DNS records for "+name, 5, 10*time.Second, func() error {
				var err error
				existing, err = api.records(ctx, zoneID, name)
				return err
			}); err != nil {
				return err
			}
			if plan := planRecords(existing, ip); len(plan.foreign) > 0 {
				foreign = append(foreign, name+": "+describe(plan.foreign))
			}
		}
		if len(foreign) > 0 {
			return fmt.Errorf("DNS records that ownstackctl did not create already exist (%s). Remove them or make them A records pointing at %s, or set %s: true to let ownstackctl replace them (then remove the setting again)",
				strings.Join(foreign, "; "), ip, replaceSetting)
		}
	}

	for _, name := range recordNames(domain) {
		rt.Logf("DNS: A %s -> %s", name, ip)
		if err := stack.Retry(ctx, rt.Console, "upsert "+name, 5, 10*time.Second, func() error {
			replaced, err := api.upsertA(ctx, zoneID, name, ip, replace)
			if len(replaced) > 0 {
				rt.Logf("DNS: replaced %s (%s is true)", describe(replaced), replaceSetting)
			}
			return err
		}); err != nil {
			return err
		}
	}
	return dnscheck.WaitForRecords(ctx, rt, 10*time.Minute)
}

// recordNames are the names the DNS contract promises: the domain and its
// wildcard.
func recordNames(domain string) []string { return []string{domain, "*." + domain} }

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
	Comment string `json:"comment,omitempty"`
}

// recordComment marks the records ownstackctl manages. Cloudflare's free plan
// allows comments of up to maxComment characters.
const (
	recordComment = "managed by ownstack"
	maxComment    = 100
)

// owned reports whether ownstackctl may change r without replaceRecords: it
// carries ownstackctl's comment, or it is an A record that already points at
// the server (adopted, for records written before comments were used).
func (r record) owned(ip string) bool {
	return strings.Contains(r.Comment, recordComment) || (r.Type == "A" && r.Content == ip)
}

// recordPlan is what upsertA does for one name.
type recordPlan struct {
	keep    *record  // existing A record updated in place; nil creates one
	want    record   // the A record the name ends up with
	remove  []record // the name's other A, AAAA, and CNAME records
	foreign []record // records among keep and remove that ownstackctl did not create
}

// unchanged reports whether the name already has exactly the wanted record.
func (p recordPlan) unchanged() bool {
	return p.keep != nil && len(p.remove) == 0 && p.keep.Content == p.want.Content &&
		p.keep.Proxied == p.want.Proxied && p.keep.Comment == p.want.Comment
}

// planRecords decides how a name comes to resolve only to ip: it keeps one A
// record (preferring one that already points at ip, then one ownstackctl
// owns) and removes every other A, AAAA, and CNAME record. Other record
// types (MX, TXT) are never touched. The choice does not depend on the order
// the API lists records in.
func planRecords(existing []record, ip string) recordPlan {
	var relevant []record
	for _, r := range existing {
		if r.Type == "A" || r.Type == "AAAA" || r.Type == "CNAME" {
			relevant = append(relevant, r)
		}
	}
	rank := func(r record) int {
		switch {
		case r.Type != "A":
			return 3
		case r.Content == ip:
			return 0
		case r.owned(ip):
			return 1
		}
		return 2
	}
	sort.SliceStable(relevant, func(i, j int) bool {
		if ri, rj := rank(relevant[i]), rank(relevant[j]); ri != rj {
			return ri < rj
		}
		return relevant[i].ID < relevant[j].ID
	})

	var plan recordPlan
	for i, r := range relevant {
		if !r.owned(ip) {
			plan.foreign = append(plan.foreign, r)
		}
		if i == 0 && r.Type == "A" {
			plan.keep = &relevant[i]
			continue
		}
		plan.remove = append(plan.remove, r)
	}
	plan.want = record{Type: "A", Content: ip, TTL: 1, Proxied: false, Comment: recordComment}
	if plan.keep != nil && plan.keep.owned(ip) {
		plan.want.Comment = ownedComment(plan.keep.Comment)
	}
	return plan
}

// ownedComment adds ownstackctl's marker to a comment someone else wrote,
// keeping their text. A comment too long to extend stays as it is; the
// record then stays ownstackctl's only while it points at the server.
func ownedComment(existing string) string {
	switch {
	case strings.Contains(existing, recordComment):
		return existing
	case existing == "":
		return recordComment
	case len(existing)+len("; ")+len(recordComment) <= maxComment:
		return existing + "; " + recordComment
	}
	return existing
}

// describe lists records for messages, such as "A 198.51.100.1, CNAME x.net".
func describe(records []record) string {
	var parts []string
	for _, r := range records {
		parts = append(parts, r.Type+" "+r.Content)
	}
	return strings.Join(parts, ", ")
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

// records lists the records for one name.
func (c *client) records(ctx context.Context, zoneID, name string) ([]record, error) {
	var existing []record
	path := fmt.Sprintf("/zones/%s/dns_records?name=%s&per_page=100", zoneID, url.QueryEscape(name))
	return existing, c.do(ctx, http.MethodGet, path, nil, &existing)
}

// upsertA makes name resolve only to ip (see planRecords). Records
// ownstackctl did not create are replaced only when replace is set, and
// returned so the caller can report them; otherwise nothing is changed.
func (c *client) upsertA(ctx context.Context, zoneID, name, ip string, replace bool) ([]record, error) {
	existing, err := c.records(ctx, zoneID, name)
	if err != nil {
		return nil, err
	}
	plan := planRecords(existing, ip)
	if len(plan.foreign) > 0 && !replace {
		return nil, fmt.Errorf("%s has records ownstackctl did not create (%s); set %s: true to replace them", name, describe(plan.foreign), replaceSetting)
	}
	// Delete first: Cloudflare refuses an A record next to a CNAME.
	for _, r := range plan.remove {
		if err := c.do(ctx, http.MethodDelete, fmt.Sprintf("/zones/%s/dns_records/%s", zoneID, r.ID), nil, nil); err != nil {
			return nil, err
		}
	}
	want := plan.want
	want.Name = name
	switch {
	case plan.unchanged():
	case plan.keep != nil:
		// PATCH keeps what ownstackctl does not manage, such as tags.
		err = c.do(ctx, http.MethodPatch, fmt.Sprintf("/zones/%s/dns_records/%s", zoneID, plan.keep.ID), want, nil)
	default:
		err = c.do(ctx, http.MethodPost, fmt.Sprintf("/zones/%s/dns_records", zoneID), want, nil)
	}
	if err != nil {
		return nil, err
	}
	return plan.foreign, nil
}
