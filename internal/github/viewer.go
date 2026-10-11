package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Token sources reported by TokenSource; the token itself is never returned to callers that
// display the status.
const (
	SourceEnv   = "GITHUB_TOKEN"
	SourceGhCLI = "gh auth token"
)

// TokenSource returns the token and where it came from: GITHUB_TOKEN wins over the gh CLI.
func TokenSource(ctx context.Context) (token, source string, err error) {
	if t := strings.TrimSpace(os.Getenv("GITHUB_TOKEN")); t != "" {
		return t, SourceEnv, nil
	}
	out, err := exec.CommandContext(ctx, "gh", "auth", "token").Output()
	if err != nil {
		return "", "", errNoToken
	}
	t := strings.TrimSpace(string(out))
	if t == "" {
		return "", "", errEmptyGhToken
	}
	return t, SourceGhCLI, nil
}

// Limit is one rate-limit bucket.
type Limit struct {
	Limit     int       `json:"limit"`
	Remaining int       `json:"remaining"`
	Reset     time.Time `json:"reset"`
}

// Viewer describes the authenticated account. Scopes is nil for fine-grained tokens and GitHub
// App tokens, which do not report OAuth scopes.
type Viewer struct {
	Login   string   `json:"login"`
	Scopes  []string `json:"scopes"`
	Core    Limit    `json:"core"`
	GraphQL Limit    `json:"graphql"`
}

// Viewer reads /user and /rate_limit (the latter does not count against the limit).
func (c *Client) Viewer(ctx context.Context) (Viewer, error) {
	var v Viewer
	body, hdr, err := c.do(ctx, http.MethodGet, "/user", nil, "")
	if err != nil {
		return v, err
	}
	var user struct {
		Login string `json:"login"`
	}
	if err := json.Unmarshal(body, &user); err != nil {
		return v, fmt.Errorf("decode /user: %w", err)
	}
	v.Login = user.Login
	if h, ok := hdr[http.CanonicalHeaderKey("X-OAuth-Scopes")]; ok && len(h) > 0 {
		v.Scopes = []string{}
		for _, s := range strings.Split(h[0], ",") {
			if s = strings.TrimSpace(s); s != "" {
				v.Scopes = append(v.Scopes, s)
			}
		}
	}
	body, _, err = c.do(ctx, http.MethodGet, "/rate_limit", nil, "")
	if err != nil {
		return v, err
	}
	var rl struct {
		Resources map[string]struct {
			Limit     int   `json:"limit"`
			Remaining int   `json:"remaining"`
			Reset     int64 `json:"reset"`
		} `json:"resources"`
	}
	if err := json.Unmarshal(body, &rl); err != nil {
		return v, fmt.Errorf("decode /rate_limit: %w", err)
	}
	conv := func(name string) Limit {
		r := rl.Resources[name]
		return Limit{Limit: r.Limit, Remaining: r.Remaining, Reset: time.Unix(r.Reset, 0)}
	}
	v.Core, v.GraphQL = conv("core"), conv("graphql")
	return v, nil
}
