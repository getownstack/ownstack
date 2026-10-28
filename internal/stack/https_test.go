package stack

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// httptest's certificate is not publicly trusted, like Let's Encrypt staging.
func TestRuntimeCheckHTTPSUntrustedCerts(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	host := srv.Listener.Addr().String()
	ctx := context.Background()

	rt := &Runtime{Out: &Outputs{}} // no compute or ingress output yet
	if c := rt.CheckHTTPS(ctx, host, "/"); c.State != Warn || !strings.Contains(c.Detail, "untrusted certificate") {
		t.Errorf("trusted-only check = %+v", c)
	}
	rt.Out.Ingress = &IngressOutput{UntrustedCerts: true}
	if c := rt.CheckHTTPS(ctx, host, "/"); c.State != OK || !strings.Contains(c.Detail, "staging certificate") {
		t.Errorf("staging check = %+v", c)
	}
}

// CheckLogin is OK only for HTTP 200 as the user behind an accepted
// certificate; any other answer fails, whatever the certificate.
func TestCheckLogin(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if user, pass, _ := r.BasicAuth(); user != "ownstack" || pass != "zot-pw" {
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	defer srv.Close()
	host := srv.Listener.Addr().String()
	ctx := context.Background()

	rt := &Runtime{Out: &Outputs{}}
	if c := rt.CheckLogin(ctx, host, "/v2/", "ownstack", "zot-pw"); c.State != Warn || !strings.HasPrefix(c.Detail, "HTTP 200 as ownstack, untrusted certificate") {
		t.Errorf("untrusted certificate = %+v", c)
	}
	if c := rt.CheckLogin(ctx, host, "/v2/", "ownstack", "wrong"); c.State != Fail || !strings.HasPrefix(c.Detail, "HTTP 401 as ownstack") {
		t.Errorf("rejected credential = %+v", c)
	}
	rt.Out.Ingress = &IngressOutput{UntrustedCerts: true}
	if c := rt.CheckLogin(ctx, host, "/v2/", "ownstack", "zot-pw"); c.State != OK || c.Name != "https://"+host {
		t.Errorf("accepted = %+v", c)
	}
	if c := rt.CheckHTTPS(ctx, host, "/v2/"); c.State != OK || !strings.HasPrefix(c.Detail, "HTTP 401, staging certificate") {
		t.Errorf("without a login a 401 answers: %+v", c)
	}
	for _, c := range []Check{rt.CheckLogin(ctx, host, "/v2/", "ownstack", "zot-pw"), rt.CheckLogin(ctx, host, "/v2/", "ownstack", "wrong")} {
		if strings.Contains(c.Detail, "zot-pw") || strings.Contains(c.Detail, "wrong") {
			t.Errorf("detail contains the password: %q", c.Detail)
		}
	}
}
