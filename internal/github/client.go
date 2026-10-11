// Package github talks to the GitHub GraphQL and REST APIs with rate-limit awareness and caching.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const (
	defaultBase     = "https://api.github.com"
	maxAttempts     = 5
	maxWait         = time.Hour
	lowRemaining    = 20
	backoffBase     = 2 * time.Second
	requestTimeout  = 60 * time.Second
	maxResponseSize = 32 << 20
)

// Cached is a stored REST response: body plus the headers needed to replay it.
type Cached struct {
	ETag, Link string
	Body       []byte
}

// Cache stores REST bodies by URL with their ETag and Link headers. Bodies never contain the token.
type Cache interface {
	Get(ctx context.Context, url string) (Cached, bool)
	Put(ctx context.Context, url string, c Cached) error
}

type Client struct {
	base             string
	token            string
	http             *http.Client
	cache            Cache
	log              io.Writer
	sleep            func(context.Context, time.Duration) error
	now              func() time.Time
	calls, cacheHits atomic.Int64
}

// Stats returns HTTP requests sent and 304 answers served from the cache.
func (c *Client) Stats() (calls, cacheHits int64) { return c.calls.Load(), c.cacheHits.Load() }

var (
	errNoToken      = errors.New("GitHub token není k dispozici (nastav GITHUB_TOKEN nebo se přihlas přes `gh auth login`)")
	errEmptyGhToken = errors.New("gh auth token vrátil prázdný token")
)

// Token returns GITHUB_TOKEN or the gh CLI token. It is never printed.
func Token(ctx context.Context) (string, error) {
	t, _, err := TokenSource(ctx)
	return t, err
}

func New(token string, cache Cache, log io.Writer) *Client {
	c := &Client{base: defaultBase, token: token, cache: cache, log: log, sleep: sleepCtx, now: time.Now}
	c.http = &http.Client{Timeout: requestTimeout, CheckRedirect: c.sameHostOnly}
	return c
}

// WithBase points the client at another API root (a test server); redirects stay on that host.
func (c *Client) WithBase(base string) *Client {
	c.base = strings.TrimSuffix(base, "/")
	return c
}

// maxRedirects matches net/http's default, which a custom CheckRedirect replaces.
const maxRedirects = 10

// sameHostOnly keeps every request on the API host and scheme: nothing else may be contacted,
// and the bearer token never travels over plain HTTP.
func (c *Client) sameHostOnly(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return fmt.Errorf("příliš mnoho přesměrování (%d)", len(via))
	}
	base, err := url.Parse(c.base)
	if err != nil || req.URL.Host != base.Host || req.URL.Scheme != base.Scheme {
		return fmt.Errorf("přesměrování mimo %s odmítnuto", c.base)
	}
	return nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// APIError is returned for non-retryable HTTP failures. It deliberately omits headers.
type APIError struct {
	Status int
	Path   string
	Type   string // GraphQL error type of the first error, e.g. NOT_FOUND
	Msg    string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("GitHub %s: HTTP %d: %s", e.Path, e.Status, e.Msg)
}

// GraphQL runs a query and decodes data into out.
func (c *Client) GraphQL(ctx context.Context, query string, vars map[string]any, out any) error {
	payload, err := json.Marshal(map[string]any{"query": query, "variables": vars})
	if err != nil {
		return fmt.Errorf("encode graphql: %w", err)
	}
	for attempt := 1; ; attempt++ {
		body, _, err := c.do(ctx, http.MethodPost, "/graphql", payload, "")
		if err != nil {
			return err
		}
		var env struct {
			Data   json.RawMessage `json:"data"`
			Errors []struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"errors"`
		}
		if err := json.Unmarshal(body, &env); err != nil {
			return fmt.Errorf("decode graphql: %w", err)
		}
		if len(env.Errors) > 0 {
			if env.Errors[0].Type == "RATE_LIMITED" && attempt < maxAttempts {
				if err := c.wait(ctx, time.Minute, "GraphQL rate limit"); err != nil {
					return err
				}
				continue
			}
			msgs := make([]string, 0, len(env.Errors))
			for _, e := range env.Errors {
				msgs = append(msgs, e.Message)
			}
			return &APIError{Status: http.StatusOK, Path: "/graphql", Type: env.Errors[0].Type, Msg: strings.Join(msgs, "; ")}
		}
		if err := json.Unmarshal(env.Data, out); err != nil {
			return fmt.Errorf("decode graphql data: %w", err)
		}
		return nil
	}
}

// GetJSON fetches a REST path, using the ETag cache, and decodes it into out.
// It returns the Link header's next URL path (empty when there is no next page).
func (c *Client) GetJSON(ctx context.Context, path string, out any) (string, error) {
	var cached Cached
	haveCache := false
	// Only stable first pages benefit from ETags; later pages shift as new items arrive.
	useCache := c.cache != nil && isFirstPage(path)
	if useCache {
		cached, haveCache = c.cache.Get(ctx, path)
	}
	etag := ""
	if haveCache {
		etag = cached.ETag
	}
	body, hdr, err := c.do(ctx, http.MethodGet, path, nil, etag)
	if err != nil {
		return "", err
	}
	link := hdr.Get("Link")
	switch {
	case body == nil && haveCache:
		// 304: replay the stored body and its Link header, which a 304 may omit.
		c.cacheHits.Add(1)
		body, link = cached.Body, cached.Link
	case body == nil:
		return "", fmt.Errorf("GitHub %s: 304 bez uložené odpovědi", path)
	case useCache && hdr.Get("ETag") != "":
		if err := c.cache.Put(ctx, path, Cached{ETag: hdr.Get("ETag"), Link: link, Body: body}); err != nil {
			return "", fmt.Errorf("cache %s: %w", path, err)
		}
	}
	if err := json.Unmarshal(body, out); err != nil {
		return "", fmt.Errorf("decode %s: %w", path, err)
	}
	return nextLink(link, c.base), nil
}

// do sends one logical request, retrying on rate limits, 5xx and non-JSON bodies.
// A nil body with nil error means 304 Not Modified.
func (c *Client) do(ctx context.Context, method, path string, payload []byte, etag string) ([]byte, http.Header, error) {
	return c.doAccept(ctx, method, path, payload, etag, acceptJSON)
}

const (
	acceptJSON = "application/vnd.github+json"
	acceptRaw  = "application/vnd.github.raw"
)

// doAccept is do with another media type (raw file contents).
func (c *Client) doAccept(ctx context.Context, method, path string, payload []byte, etag, accept string) ([]byte, http.Header, error) {
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		req, err := http.NewRequestWithContext(ctx, method, c.base+path, bytes.NewReader(payload))
		if err != nil {
			return nil, nil, fmt.Errorf("build request %s: %w", path, err)
		}
		req.Header.Set("Authorization", "Bearer "+c.token)
		req.Header.Set("Accept", accept)
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		if etag != "" {
			req.Header.Set("If-None-Match", etag)
		}
		c.calls.Add(1)
		resp, err := c.http.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("GitHub %s: %w", path, redact(err, c.token))
			if werr := c.wait(ctx, backoffBase*time.Duration(attempt), "síťová chyba"); werr != nil {
				return nil, nil, werr
			}
			continue
		}
		body, rerr := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize))
		resp.Body.Close()
		if rerr != nil {
			lastErr = fmt.Errorf("read %s: %w", path, rerr)
			continue
		}
		retryAfter, retry := c.retryDelay(resp, body, attempt)
		if retry {
			lastErr = &APIError{Status: resp.StatusCode, Path: path, Msg: snippet(body)}
			if werr := c.wait(ctx, retryAfter, fmt.Sprintf("HTTP %d na %s", resp.StatusCode, path)); werr != nil {
				return nil, nil, werr
			}
			continue
		}
		if resp.StatusCode == http.StatusNotModified {
			if err := c.throttle(ctx, resp.Header); err != nil {
				return nil, nil, err
			}
			return nil, resp.Header, nil
		}
		if resp.StatusCode >= 300 {
			return nil, nil, &APIError{Status: resp.StatusCode, Path: path, Msg: snippet(body)}
		}
		if err := c.throttle(ctx, resp.Header); err != nil {
			return nil, nil, err
		}
		return body, resp.Header, nil
	}
	return nil, nil, fmt.Errorf("vzdávám po %d pokusech: %w", maxAttempts, lastErr)
}

// retryDelay decides whether a response should be retried and after how long.
func (c *Client) retryDelay(resp *http.Response, body []byte, attempt int) (time.Duration, bool) {
	h := resp.Header
	switch {
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusForbidden:
		if s := h.Get("Retry-After"); s != "" {
			if n, err := strconv.Atoi(s); err == nil {
				return time.Duration(n) * time.Second, true
			}
		}
		if h.Get("X-Ratelimit-Remaining") == "0" {
			return c.untilReset(h), true
		}
		if bytes.Contains(bytes.ToLower(body), []byte("secondary rate limit")) {
			return time.Minute, true
		}
		return 0, false
	case resp.StatusCode >= 500:
		return backoffBase * time.Duration(attempt), true
	case resp.StatusCode == http.StatusOK && !json.Valid(body):
		// GitHub occasionally answers heavy queries with an HTML error page.
		return backoffBase * time.Duration(attempt), true
	}
	return 0, false
}

// throttle sleeps until reset when the budget is exhausted, so the next call does not fail.
func (c *Client) throttle(ctx context.Context, h http.Header) error {
	rem, err := strconv.Atoi(h.Get("X-Ratelimit-Remaining"))
	if err != nil {
		return nil // no rate-limit headers (e.g. tests or GHES without limits)
	}
	if rem == lowRemaining {
		fmt.Fprintf(c.log, "  GitHub: zbývá %d požadavků do resetu\n", rem)
	}
	if rem > 0 {
		return nil
	}
	return c.wait(ctx, c.untilReset(h), "vyčerpaný rate limit")
}

func (c *Client) untilReset(h http.Header) time.Duration {
	reset, err := strconv.ParseInt(h.Get("X-Ratelimit-Reset"), 10, 64)
	if err != nil {
		return time.Minute
	}
	d := time.Unix(reset, 0).Add(time.Second).Sub(c.now())
	if d < time.Second {
		d = time.Second
	}
	return d
}

func (c *Client) wait(ctx context.Context, d time.Duration, why string) error {
	if d > maxWait {
		return fmt.Errorf("GitHub: %s, čekání %s přesahuje limit %s", why, d.Round(time.Second), maxWait)
	}
	fmt.Fprintf(c.log, "  GitHub: %s — čekám %s\n", why, d.Round(time.Second))
	return c.sleep(ctx, d)
}

func snippet(b []byte) string {
	const maxLen = 200
	var m struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(b, &m) == nil && m.Message != "" {
		return m.Message
	}
	s := strings.TrimSpace(string(b))
	if len(s) > maxLen {
		s = s[:maxLen] + "…"
	}
	return s
}

func redact(err error, token string) error {
	if token == "" || !strings.Contains(err.Error(), token) {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), token, "***"))
}

// isFirstPage reports whether a REST path has no explicit page parameter.
func isFirstPage(path string) bool {
	u, err := url.Parse(path)
	return err == nil && !u.Query().Has("page")
}

// nextLink extracts rel="next" from a Link header as a path relative to base.
func nextLink(link, base string) string {
	for part := range strings.SplitSeq(link, ",") {
		segs := strings.Split(part, ";")
		if len(segs) < 2 || !strings.Contains(segs[1], `rel="next"`) {
			continue
		}
		u := strings.Trim(strings.TrimSpace(segs[0]), "<>")
		rest, ok := strings.CutPrefix(u, base)
		if !ok || !strings.HasPrefix(rest, "/") {
			return "" // never follow a pagination link to another host
		}
		return rest
	}
	return ""
}
