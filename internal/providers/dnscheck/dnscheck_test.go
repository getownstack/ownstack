package dnscheck

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"

	"github.com/getownstack/ownstack/internal/providers/providertest"
	"github.com/getownstack/ownstack/internal/stack"
)

// fakeDNS answers from records keyed by "server network name". A missing
// entry is NXDOMAIN; servers listed in down time out.
type fakeDNS struct {
	records map[string][]string
	down    map[string]bool
	asked   []string
}

func (f *fakeDNS) install(t *testing.T) {
	t.Helper()
	oldLookup, oldLocal := lookupIP, localAddrs
	t.Cleanup(func() { lookupIP, localAddrs = oldLookup, oldLocal })
	lookupIP = func(_ context.Context, server, network, name string) ([]net.IP, error) {
		f.asked = append(f.asked, server+" "+network+" "+name)
		if f.down[server] {
			return nil, &net.DNSError{Err: "i/o timeout", Name: name, Server: server, IsTimeout: true}
		}
		addrs, ok := f.records[network+" "+name]
		if !ok {
			return nil, &net.DNSError{Err: "no such host", Name: name, Server: server, IsNotFound: true}
		}
		var ips []net.IP
		for _, a := range addrs {
			ips = append(ips, net.ParseIP(a))
		}
		return ips, nil
	}
	localAddrs = func() ([]net.Addr, error) {
		return []net.Addr{
			&net.IPNet{IP: net.ParseIP("203.0.113.10"), Mask: net.CIDRMask(24, 32)},
			&net.IPNet{IP: net.ParseIP("2001:db8::10"), Mask: net.CIDRMask(64, 128)},
			&net.IPNet{IP: net.ParseIP("fe80::1"), Mask: net.CIDRMask(64, 128)},
		}, nil
	}
}

const wildcard = ProbeLabel + ".example.com"

func checks(t *testing.T) []stack.Check {
	t.Helper()
	rt, _ := providertest.Runtime(t)
	return Checks(context.Background(), rt)
}

func TestExactMatch(t *testing.T) {
	f := &fakeDNS{records: map[string][]string{
		"ip4 example.com": {"203.0.113.10"},
		"ip4 " + wildcard: {"203.0.113.10"},
	}}
	f.install(t)
	for _, c := range checks(t) {
		if c.State != stack.OK || c.Detail != "resolves to 203.0.113.10" {
			t.Errorf("%+v", c)
		}
	}
}

func TestExtraAddressesAndMissingNames(t *testing.T) {
	f := &fakeDNS{records: map[string][]string{
		"ip4 example.com": {"203.0.113.10", "198.51.100.1"},
	}}
	f.install(t)
	got := checks(t)
	if got[0].State != stack.Fail || got[0].Detail != "resolves to 198.51.100.1, 203.0.113.10" {
		t.Errorf("apex: %+v", got[0])
	}
	if got[1].State != stack.Fail || got[1].Detail != "does not resolve" {
		t.Errorf("wildcard: %+v", got[1])
	}
	// NXDOMAIN is an answer: the second resolver is not asked.
	for _, q := range f.asked {
		if strings.HasPrefix(q, "8.8.8.8") {
			t.Errorf("asked the fallback resolver: %s", q)
		}
	}
}

func TestResolverFallback(t *testing.T) {
	f := &fakeDNS{
		records: map[string][]string{"ip4 example.com": {"203.0.113.10"}},
		down:    map[string]bool{"1.1.1.1:53": true},
	}
	f.install(t)
	if ok, detail := Matches(context.Background(), "example.com", "203.0.113.10"); !ok {
		t.Errorf("fallback resolver not used: %s", detail)
	}

	f.down["8.8.8.8:53"] = true
	ok, detail := Matches(context.Background(), "example.com", "203.0.113.10")
	if ok || !strings.HasPrefix(detail, "DNS query to public resolvers failed") {
		t.Errorf("ok=%v detail=%q, want a resolver failure, not 'does not resolve'", ok, detail)
	}
	if missing := mismatches(context.Background(), "example.com", "203.0.113.10"); len(missing) != 2 || !strings.Contains(missing[0], "public resolvers failed") {
		t.Errorf("mismatches = %v", missing)
	}
}

func TestAAAARecords(t *testing.T) {
	f := &fakeDNS{records: map[string][]string{
		"ip4 example.com": {"203.0.113.10"},
		"ip4 " + wildcard: {"203.0.113.10"},
		// A leftover record pointing elsewhere, and the server's own address.
		"ip6 example.com": {"2001:db8::99"},
		"ip6 " + wildcard: {"2001:db8::10"},
	}}
	f.install(t)
	got := checks(t)
	if got[0].State != stack.Warn || !strings.Contains(got[0].Detail, "AAAA records point at 2001:db8::99, which is not this server") {
		t.Errorf("apex: %+v", got[0])
	}
	if got[1].State != stack.OK {
		t.Errorf("dual-stack wildcard pointing at this server: %+v", got[1])
	}
	// A failing A record stays a failure.
	delete(f.records, "ip4 example.com")
	if got := checks(t); got[0].State != stack.Fail || !strings.Contains(got[0].Detail, "AAAA") {
		t.Errorf("apex: %+v", got[0])
	}
}

func TestLookupStopsWhenCancelled(t *testing.T) {
	f := &fakeDNS{down: map[string]bool{"1.1.1.1:53": true, "8.8.8.8:53": true}}
	f.install(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Lookup(ctx, "example.com"); !errors.Is(err, errNoAnswer) {
		t.Errorf("err = %v", err)
	}
	if len(f.asked) != 1 {
		t.Errorf("asked %v after the context ended", f.asked)
	}
}
