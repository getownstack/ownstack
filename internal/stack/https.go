package stack

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

// CheckHTTPS reports whether host serves HTTPS with an issued certificate and
// what path answers. It connects to ip directly so it works before public
// DNS has propagated, and it distinguishes Traefik's placeholder certificate
// from an issued one.
func CheckHTTPS(ctx context.Context, host, ip, path string, allowUntrusted bool) Check {
	name := "https://" + host
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true, ServerName: host},
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if ip != "" {
				_, port, _ := net.SplitHostPort(addr)
				addr = net.JoinHostPort(ip, port)
			}
			return dialer.DialContext(ctx, network, addr)
		},
	}
	client := &http.Client{Timeout: 15 * time.Second, Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+host+path, nil)
	if err != nil {
		return Check{Name: name, State: Fail, Detail: err.Error()}
	}
	res, err := client.Do(req)
	if err != nil {
		return Check{Name: name, State: Fail, Detail: "unreachable: " + shortErr(err)}
	}
	defer res.Body.Close()

	if res.TLS == nil || len(res.TLS.PeerCertificates) == 0 {
		return Check{Name: name, State: Fail, Detail: "no certificate"}
	}
	leaf := res.TLS.PeerCertificates[0]
	if strings.Contains(leaf.Subject.CommonName, "TRAEFIK DEFAULT CERT") {
		return Check{Name: name, State: Warn, Detail: fmt.Sprintf("HTTP %d, certificate not issued yet", res.StatusCode)}
	}
	intermediates := x509.NewCertPool()
	for _, c := range res.TLS.PeerCertificates[1:] {
		intermediates.AddCert(c)
	}
	_, verifyErr := leaf.Verify(x509.VerifyOptions{DNSName: host, Intermediates: intermediates})
	expires := leaf.NotAfter.Format("2006-01-02")
	switch {
	case verifyErr == nil:
		return Check{Name: name, State: stateForStatus(res.StatusCode), Detail: fmt.Sprintf("HTTP %d, certificate valid until %s", res.StatusCode, expires)}
	case allowUntrusted:
		return Check{Name: name, State: stateForStatus(res.StatusCode), Detail: fmt.Sprintf("HTTP %d, staging certificate until %s", res.StatusCode, expires)}
	default:
		return Check{Name: name, State: Warn, Detail: fmt.Sprintf("HTTP %d, untrusted certificate: %s", res.StatusCode, shortErr(verifyErr))}
	}
}

func stateForStatus(code int) string {
	if code >= 500 {
		return Fail
	}
	return OK
}

func shortErr(err error) string {
	msg := err.Error()
	if i := strings.LastIndex(msg, ": "); i >= 0 && len(msg) > 120 {
		msg = msg[i+2:]
	}
	return msg
}

// ServiceClient returns an HTTP client for talking to platform services
// during bootstrap. It connects to the node's public IP for any hostname, so
// it does not depend on DNS propagation, and it accepts certificates that are
// not issued yet.
func (rt *Runtime) ServiceClient() *http.Client {
	if rt.ServiceHTTP != nil {
		return rt.ServiceHTTP
	}
	ip := ""
	if rt.Out.Compute != nil {
		ip = rt.Out.Compute.PublicIP
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	return &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				if ip != "" {
					_, port, _ := net.SplitHostPort(addr)
					addr = net.JoinHostPort(ip, port)
				}
				return dialer.DialContext(ctx, network, addr)
			},
		},
	}
}

// WaitForCertificate waits until host serves a publicly trusted certificate.
// Services other machines connect to (registries pushed to from CI) are not
// usable before that, so their stages wait instead of finishing early.
func WaitForCertificate(ctx context.Context, rt *Runtime, host, path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		check := CheckHTTPS(ctx, host, rt.Out.Compute.PublicIP, path, false)
		if check.State == OK {
			rt.Logf("%s: %s", host, check.Detail)
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s has no trusted certificate after %s (%s); check DNS and the ACME email, then rerun apply", host, timeout, check.Detail)
		}
		rt.Logf("  waiting: certificate for %s (%s)", host, check.Detail)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(15 * time.Second):
		}
	}
}
