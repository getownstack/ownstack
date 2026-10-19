// Package manualdns implements the DNS contract without any DNS API: the user
// creates the records at their DNS host, ownstackctl waits for them, and
// certificates are validated over TLS-ALPN on the node.
package manualdns

import (
	"context"
	"fmt"
	"time"

	"github.com/getownstack/ownstack-cluster-template/internal/providers/dnscheck"
	"github.com/getownstack/ownstack-cluster-template/internal/stack"
)

type Provider struct{}

func (Provider) Resolve(_ context.Context, rt *stack.Runtime) error {
	rt.Out.DNS = &stack.DNSOutput{Challenge: stack.ChallengeTLSALPN}
	return nil
}

func (Provider) Plan(ctx context.Context, rt *stack.Runtime) []stack.Step {
	domain, ip := rt.Manifest.Spec.Domain, rt.Out.Compute.PublicIP
	if dnscheck.AllMatch(ctx, domain, ip) {
		return []stack.Step{{Action: "verify", Target: "DNS records", Detail: fmt.Sprintf("%s and *.%s resolve to %s", domain, domain, ip)}}
	}
	return []stack.Step{{Action: "ensure", Target: "DNS records", Detail: fmt.Sprintf("you create A %s and A *.%s -> %s; apply waits for them", domain, domain, ip)}}
}

func (Provider) Apply(ctx context.Context, rt *stack.Runtime) error {
	domain, ip := rt.Manifest.Spec.Domain, rt.Out.Compute.PublicIP
	if !dnscheck.AllMatch(ctx, domain, ip) {
		rt.Logf("Create these records at your DNS host (not proxied):")
		rt.Logf("  A  %s    -> %s", domain, ip)
		rt.Logf("  A  *.%s  -> %s", domain, ip)
		rt.Logf("Remove any AAAA records for these names unless they point at this server.")
	}
	return dnscheck.WaitForRecords(ctx, rt, time.Duration(rt.SettingInt("waitMinutes"))*time.Minute)
}

func (Provider) Status(ctx context.Context, rt *stack.Runtime) []stack.Check {
	return dnscheck.Checks(ctx, rt)
}
