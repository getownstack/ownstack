package health

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestURLFromReport(t *testing.T) {
	if got := URLFromReport("https://app.ownstack.io/agent/jobs/abc/report"); got != "https://app.ownstack.io/agent/jobs/abc/health" {
		t.Errorf("got %q", got)
	}
	if got := URLFromReport("https://example.com/other"); got != "" {
		t.Errorf("unrelated URL mapped to %q", got)
	}
}

func TestSend(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		json.NewDecoder(r.Body).Decode(&got)
	}))
	defer srv.Close()
	if err := Send(context.Background(), srv.URL, "tok", map[string]string{"state": "ok"}); err != nil {
		t.Fatal(err)
	}
	if got["state"] != "ok" {
		t.Errorf("body = %v", got)
	}
	if err := Send(context.Background(), srv.URL, "wrong", nil); err == nil {
		t.Error("rejected token reported as success")
	}
}

func TestServiceUnitQuotesPaths(t *testing.T) {
	unit := serviceUnit("/usr/local/bin/ownstackctl", `/root/my stack/50%$"x".yaml`, "/etc/ownstack/secrets.env")
	want := `ExecStart=/usr/local/bin/ownstackctl status -f "/root/my stack/50%%$$\"x\".yaml" --secrets-file "/etc/ownstack/secrets.env" --no-prompt --health-url ${OWNSTACK_HEALTH_URL}`
	if !strings.Contains(unit, want+"\n") {
		t.Errorf("unit:\n%s\nwant line:\n%s", unit, want)
	}
}
