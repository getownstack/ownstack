// Package dnscheck verifies the DNS contract from the public internet's point
// of view, independent of which provider manages the records.
package dnscheck

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/getownstack/ownstack-cluster-template/internal/stack"
)

// ProbeLabel is resolved under the domain to check the wildcard record.
const ProbeLabel = "ownstack-wildcard-check"

// Resolver queries public resolvers directly so local caches and
// /etc/hosts entries on the node do not hide a broken record.
var Resolver = &net.Resolver{
	PreferGo: true,
	Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		d := net.Dialer{Timeout: 5 * time.Second}
		return d.DialContext(ctx, network, "1.1.1.1:53")
	},
}

// Names returns the two names the DNS contract promises: the domain and a
// name under its wildcard.
func Names(domain string) []string {
	return []string{domain, ProbeLabel + "." + domain}
}

// Lookup resolves a name to its IPv4 addresses.
func Lookup(ctx context.Context, name string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	addrs, err := Resolver.LookupIP(ctx, "ip4", name)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, a := range addrs {
		out = append(out, a.String())
	}
	sort.Strings(out)
	return out, nil
}

// Matches reports whether name resolves to exactly ip.
func Matches(ctx context.Context, name, ip string) (bool, string) {
	addrs, err := Lookup(ctx, name)
	if err != nil {
		return false, "does not resolve"
	}
	if len(addrs) == 1 && addrs[0] == ip {
		return true, "resolves to " + ip
	}
	return false, "resolves to " + strings.Join(addrs, ", ")
}

// Checks reports resolution of the domain and wildcard.
func Checks(ctx context.Context, rt *stack.Runtime) []stack.Check {
	ip := rt.Out.Compute.PublicIP
	var checks []stack.Check
	for i, name := range Names(rt.Manifest.Spec.Domain) {
		label := rt.Manifest.Spec.Domain
		if i == 1 {
			label = "*." + rt.Manifest.Spec.Domain
		}
		ok, detail := Matches(ctx, name, ip)
		state := stack.OK
		if !ok {
			state = stack.Fail
		}
		checks = append(checks, stack.Check{Name: label, State: state, Detail: detail})
	}
	return checks
}

// AllMatch reports whether both names resolve to ip.
func AllMatch(ctx context.Context, domain, ip string) bool {
	for _, name := range Names(domain) {
		if ok, _ := Matches(ctx, name, ip); !ok {
			return false
		}
	}
	return true
}

// WaitForRecords polls public DNS until both names resolve to ip.
func WaitForRecords(ctx context.Context, rt *stack.Runtime, timeout time.Duration) error {
	domain, ip := rt.Manifest.Spec.Domain, rt.Out.Compute.PublicIP
	deadline := time.Now().Add(timeout)
	for {
		if AllMatch(ctx, domain, ip) {
			rt.Logf("DNS: %s and *.%s resolve to %s", domain, domain, ip)
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s and *.%s do not resolve to %s after %s", domain, domain, ip, timeout)
		}
		rt.Logf("  waiting: %s and *.%s to resolve to %s", domain, domain, ip)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(15 * time.Second):
		}
	}
}
