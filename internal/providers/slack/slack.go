// Package slack implements the alerting contract with a Slack bot.
package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/getownstack/ownstack-cluster-template/internal/stack"
)

type Provider struct {
	// APIURL overrides the API endpoint in tests.
	APIURL string
}

func (p Provider) Resolve(_ context.Context, rt *stack.Runtime) error {
	out := &stack.AlertsOutput{Kind: "slack", Channel: rt.Setting("channel")}
	rt.Out.Alerts = out
	token, err := rt.Secret("token")
	if err != nil {
		return err
	}
	out.Token = token
	return nil
}

func (p Provider) Plan(_ context.Context, rt *stack.Runtime) []stack.Step {
	return []stack.Step{{Action: "verify", Target: "Slack bot token", Detail: "notifications go to " + rt.Out.Alerts.Channel}}
}

func (p Provider) Apply(ctx context.Context, rt *stack.Runtime) error {
	team, err := p.authTest(ctx, rt)
	if err != nil {
		return fmt.Errorf("Slack token: %w", err)
	}
	rt.Logf("Slack: bot authenticated in %s; notifications go to %s", team, rt.Out.Alerts.Channel)
	return nil
}

func (p Provider) Status(ctx context.Context, rt *stack.Runtime) []stack.Check {
	if rt.Out.Alerts.Token == "" {
		return []stack.Check{{Name: "token", State: stack.Unknown, Detail: "token not available on this node"}}
	}
	team, err := p.authTest(ctx, rt)
	if err != nil {
		return []stack.Check{{Name: "token", State: stack.Fail, Detail: err.Error()}}
	}
	return []stack.Check{{Name: "token", State: stack.OK, Detail: "bot in " + team + ", channel " + rt.Out.Alerts.Channel}}
}

func (p Provider) authTest(ctx context.Context, rt *stack.Runtime) (string, error) {
	base := p.APIURL
	if base == "" {
		base = "https://slack.com/api"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/auth.test", strings.NewReader(""))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+rt.Out.Alerts.Token)
	res, err := rt.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	var body struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
		Team  string `json:"team"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		return "", fmt.Errorf("HTTP %d", res.StatusCode)
	}
	if !body.OK {
		return "", fmt.Errorf("%s", body.Error)
	}
	return body.Team, nil
}
