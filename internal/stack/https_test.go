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
