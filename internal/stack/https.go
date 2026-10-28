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

// checkHTTPS reports whether host serves HTTPS with an issued certificate and
// what path answers. It connects to ip directly so it works before public
// DNS has propagated, and it distinguishes Traefik's placeholder certificate
// from an issued one. With a login, the request carries HTTP basic auth and
// only HTTP 200 is OK, so one request proves the credential, the route and
// the certificate.
func checkHTTPS(ctx context.Context, host, ip, path string, allowUntrusted bool, login *login) Check {
	name := "https://" + host
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	// One request per transport: no keep-alive, so polling loops (verify,
	// WaitFor) do not pile up idle connections.
	transport := &http.Transport{
		TLSClientConfig:   &tls.Config{InsecureSkipVerify: true, ServerName: host},
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if ip != "" {
				_, port, _ := net.SplitHostPort(addr)
				addr = net.JoinHostPort(ip, port)
			}
			return dialer.DialContext(ctx, network, addr)
		},
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Timeout: 15 * time.Second, Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+host+path, nil)
	if err != nil {
		return Check{Name: name, State: Fail, Detail: err.Error()}
	}
	if login != nil {
		req.SetBasicAuth(login.user, login.password)
	}
	res, err := client.Do(req)
	if err != nil {
		return Check{Name: name, State: Fail, Detail: "unreachable: " + shortErr(err)}
	}
	defer res.Body.Close()

	if res.TLS == nil || len(res.TLS.PeerCertificates) == 0 {
		return Check{Name: name, State: Fail, Detail: "no certificate"}
	}
	answer, state := fmt.Sprintf("HTTP %d", res.StatusCode), OK
	if login != nil {
		answer += " as " + login.user
	}
	if res.StatusCode >= 500 || (login != nil && res.StatusCode != http.StatusOK) {
		state = Fail
	}
	cert, accepted := certificate(res.TLS.PeerCertificates, host, allowUntrusted)
	if !accepted && state == OK {
		state = Warn
	}
	return Check{Name: name, State: state, Detail: answer + ", " + cert}
}

// certificate describes the certificate chain host served, and whether
// checks accept it: publicly trusted, or issued by an untrusted CA when
// allowUntrusted is set (Let's Encrypt staging).
func certificate(chain []*x509.Certificate, host string, allowUntrusted bool) (string, bool) {
	leaf := chain[0]
	if strings.Contains(leaf.Subject.CommonName, "TRAEFIK DEFAULT CERT") {
		return "certificate not issued yet", false
	}
	intermediates := x509.NewCertPool()
	for _, c := range chain[1:] {
		intermediates.AddCert(c)
	}
	expires := leaf.NotAfter.Format("2006-01-02")
	_, err := leaf.Verify(x509.VerifyOptions{DNSName: host, Intermediates: intermediates})
	switch {
	case err == nil:
		return "certificate valid until " + expires, true
	case allowUntrusted:
		return "staging certificate until " + expires, true
	}
	return "untrusted certificate: " + shortErr(err), false
}

type login struct{ user, password string }

// CheckHTTPS checks host on this stack's node. It accepts certificates the
// ingress marks as untrusted (Let's Encrypt staging), so staging stacks can
// become healthy.
func (rt *Runtime) CheckHTTPS(ctx context.Context, host, path string) Check {
	return checkHTTPS(ctx, host, rt.publicIP(), path, rt.untrustedCerts(), nil)
}

// CheckLogin is CheckHTTPS for a request with HTTP basic auth as user. It
// is OK only when host answers HTTP 200 behind an issued certificate.
func (rt *Runtime) CheckLogin(ctx context.Context, host, path, user, password string) Check {
	return checkHTTPS(ctx, host, rt.publicIP(), path, rt.untrustedCerts(), &login{user, password})
}

func (rt *Runtime) publicIP() string {
	if rt.Out.Compute == nil {
		return ""
	}
	return rt.Out.Compute.PublicIP
}

func (rt *Runtime) untrustedCerts() bool {
	return rt.Out.Ingress != nil && rt.Out.Ingress.UntrustedCerts
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
	ip := rt.publicIP()
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	return &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			// Callers build a client per attempt; idle connections must not
			// outlive the retry loop.
			IdleConnTimeout: 30 * time.Second,
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
