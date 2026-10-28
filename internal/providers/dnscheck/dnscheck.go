// Package dnscheck verifies the DNS contract from the public internet's point
// of view, independent of which provider manages the records.
package dnscheck

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/getownstack/ownstack/internal/stack"
)

// ProbeLabel is resolved under the domain to check the wildcard record.
const ProbeLabel = "ownstack-wildcard-check"

// Servers are the public resolvers queried, in order. They are asked
// directly so local caches and /etc/hosts entries on the node do not hide a
// broken record. The next one is only asked when a query gets no answer.
var Servers = []string{"1.1.1.1:53", "8.8.8.8:53"}

// lookupIP resolves name at one server; tests replace it.
var lookupIP = func(ctx context.Context, server, network, name string) ([]net.IP, error) {
	r := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			d := net.Dialer{Timeout: 5 * time.Second}
			return d.DialContext(ctx, network, server)
		},
	}
	return r.LookupIP(ctx, network, name)
}

// localAddrs lists the node's own addresses; tests replace it.
var localAddrs = net.InterfaceAddrs

// errNoAnswer means no public resolver answered, which says nothing about
// the records themselves.
var errNoAnswer = errors.New("DNS query to public resolvers failed")

// Names returns the two names the DNS contract promises: the domain and a
// name under its wildcard.
func Names(domain string) []string {
	return []string{domain, ProbeLabel + "." + domain}
}

// Lookup resolves a name to its IPv4 addresses.
func Lookup(ctx context.Context, name string) ([]string, error) {
	return lookup(ctx, "ip4", name)
}

// lookup asks each server in turn until one answers. A name that does not
// exist is an answer; a timeout or a network error moves on to the next
// server, and when none answers the error wraps errNoAnswer.
func lookup(ctx context.Context, network, name string) ([]string, error) {
	var last error
	for _, server := range Servers {
		qctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		addrs, err := lookupIP(qctx, server, network, name)
		cancel()
		if err == nil {
			var out []string
			for _, a := range addrs {
				out = append(out, a.String())
			}
			sort.Strings(out)
			return out, nil
		}
		var dnsErr *net.DNSError
		if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
			return nil, err
		}
		last = err
		if ctx.Err() != nil {
			break
		}
	}
	return nil, fmt.Errorf("%w: %v", errNoAnswer, last)
}

// Matches reports whether name resolves to exactly ip.
func Matches(ctx context.Context, name, ip string) (bool, string) {
	addrs, err := Lookup(ctx, name)
	switch {
	case errors.Is(err, errNoAnswer):
		return false, err.Error()
	case err != nil:
		return false, "does not resolve"
	case len(addrs) == 1 && addrs[0] == ip:
		return true, "resolves to " + ip
	}
	return false, "resolves to " + strings.Join(addrs, ", ")
}

// foreignAAAA returns the name's IPv6 addresses that are not this server's.
// The compute contract is IPv4, but Let's Encrypt and IPv6 visitors use AAAA
// records when they exist. Lookup failures report nothing.
func foreignAAAA(ctx context.Context, name string) []string {
	addrs, err := lookup(ctx, "ip6", name)
	if err != nil || len(addrs) == 0 {
		return nil
	}
	own := map[string]bool{}
	if local, err := localAddrs(); err == nil {
		for _, a := range local {
			if ipnet, ok := a.(*net.IPNet); ok && ipnet.IP.To4() == nil && ipnet.IP.IsGlobalUnicast() {
				own[ipnet.IP.String()] = true
			}
		}
	}
	var foreign []string
	for _, a := range addrs {
		if !own[a] {
			foreign = append(foreign, a)
		}
	}
	return foreign
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
		if aaaa := foreignAAAA(ctx, name); len(aaaa) > 0 {
			if state == stack.OK {
				state = stack.Warn
			}
			detail += fmt.Sprintf("; AAAA records point at %s, which is not this server: remove or fix them (Let's Encrypt and IPv6 visitors use them)", strings.Join(aaaa, ", "))
		}
		checks = append(checks, stack.Check{Name: label, State: state, Detail: detail})
	}
	return checks
}

// AllMatch reports whether both names resolve to ip.
func AllMatch(ctx context.Context, domain, ip string) bool {
	return len(mismatches(ctx, domain, ip)) == 0
}

// mismatches describes each name that does not resolve to ip.
func mismatches(ctx context.Context, domain, ip string) []string {
	var out []string
	for i, name := range Names(domain) {
		if ok, detail := Matches(ctx, name, ip); !ok {
			label := domain
			if i == 1 {
				label = "*." + domain
			}
			out = append(out, label+" "+detail)
		}
	}
	return out
}

// WaitForRecords polls public DNS until both names resolve to ip.
func WaitForRecords(ctx context.Context, rt *stack.Runtime, timeout time.Duration) error {
	domain, ip := rt.Manifest.Spec.Domain, rt.Out.Compute.PublicIP
	deadline := time.Now().Add(timeout)
	for {
		missing := mismatches(ctx, domain, ip)
		if len(missing) == 0 {
			rt.Logf("DNS: %s and *.%s resolve to %s", domain, domain, ip)
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s and *.%s do not resolve to %s after %s (%s)", domain, domain, ip, timeout, strings.Join(missing, "; "))
		}
		rt.Logf("  waiting: %s and *.%s to resolve to %s", domain, domain, ip)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(15 * time.Second):
		}
	}
}
