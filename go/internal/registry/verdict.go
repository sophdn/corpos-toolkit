package registry

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Verdict is the CI validity-stamp outcome for a mirrored push — the payload
// of the forge-v2 "completion ping" (§2): after [Mirror] pushes (without
// blocking the agent's record() return), CI re-validates + stamps the commit,
// and this is the verdict that re-enters the agent's context.
type Verdict struct {
	SHA         string `json:"sha"`
	State       string `json:"state"` // "success" (blessed) | "failure" | "error" | "pending"
	Description string `json:"description,omitempty"`
}

// Blessed reports whether the registry CI stamped the commit valid.
func (v Verdict) Blessed() bool { return v.State == "success" }

// StatusFetcher returns the CI commit-status state + description for a commit
// SHA. Abstracted so [PollVerdict] is unit-testable without the live Gitea
// (the test injects a scripted fetcher); [GiteaStatusFetcher] is the real one.
type StatusFetcher interface {
	FetchStatus(ctx context.Context, sha string) (state, description string, err error)
}

// PollVerdict polls the CI status for a mirrored commit until it resolves to
// a terminal state (success / failure / error) or attempts run out. This is
// the async completion-ping tail: the agent's record() return never waited on
// CI; a save-boundary hook calls Mirror then PollVerdict and surfaces the
// result. A still-pending poll-out returns State="pending" (not an error) so
// the caller can re-ping later rather than treating slow CI as a failure.
func PollVerdict(ctx context.Context, fetcher StatusFetcher, sha string, attempts int, interval time.Duration) (Verdict, error) {
	if attempts < 1 {
		attempts = 1
	}
	var last Verdict
	for i := 0; i < attempts; i++ {
		state, desc, err := fetcher.FetchStatus(ctx, sha)
		if err != nil {
			return Verdict{}, fmt.Errorf("fetch CI status for %s: %w", sha, err)
		}
		last = Verdict{SHA: sha, State: state, Description: desc}
		switch state {
		case "success", "failure", "error":
			return last, nil
		}
		if i == attempts-1 {
			break
		}
		select {
		case <-ctx.Done():
			return last, ctx.Err()
		case <-time.After(interval):
		}
	}
	if last.State == "" {
		last = Verdict{SHA: sha, State: "pending"}
	}
	return last, nil
}

// GiteaStatusFetcher reads the combined commit status from a Gitea repo via
// its API (GET /repos/{owner}/{repo}/commits/{sha}/status). The token doubles
// as the API key via the `Authorization: token` header. InsecureTLS covers
// the homelab self-signed cert.
type GiteaStatusFetcher struct {
	APIBase  string // e.g. https://example-host.local/git/api/v1
	Owner    string
	Repo     string
	Token    string
	Insecure bool
	client   *http.Client
}

// FetchStatus implements [StatusFetcher] against the live Gitea API.
func (g *GiteaStatusFetcher) FetchStatus(ctx context.Context, sha string) (string, string, error) {
	if g.client == nil {
		tr := &http.Transport{}
		if g.Insecure {
			tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // homelab self-signed cert, opt-in via Insecure
		}
		g.client = &http.Client{Transport: tr, Timeout: 15 * time.Second}
	}
	url := fmt.Sprintf("%s/repos/%s/%s/commits/%s/status",
		strings.TrimRight(g.APIBase, "/"), g.Owner, g.Repo, sha)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", "", err
	}
	if g.Token != "" {
		req.Header.Set("Authorization", "token "+g.Token)
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("gitea status: HTTP %d", resp.StatusCode)
	}
	var body struct {
		State    string `json:"state"`
		Statuses []struct {
			Description string `json:"description"`
		} `json:"statuses"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", "", fmt.Errorf("decode gitea status: %w", err)
	}
	desc := ""
	if len(body.Statuses) > 0 {
		desc = body.Statuses[0].Description
	}
	return body.State, desc, nil
}
