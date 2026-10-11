package github

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"time"
)

// The public price list maintained by the LiteLLM project, read through the GitHub API (the only
// network access repolens has). It lists USD per token for thousands of models.
const (
	priceRepo = "BerriAI/litellm"
	priceFile = "model_prices_and_context_window.json"
	// PriceListSource names the list in the dashboard.
	PriceListSource = "LiteLLM (" + priceRepo + ")"
	perMillion      = 1e6
	// maxPrice bounds USD per million tokens; anything above is a broken entry.
	maxPrice = 1e6
)

// ModelPrice is USD per million tokens.
type ModelPrice struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read"`
	CacheWrite float64 `json:"cache_write"`
}

// PriceList is the fetched list: prices by exact model name and the date of the file's last change.
type PriceList struct {
	Date   string                `json:"date"` // YYYY-MM-DD
	Prices map[string]ModelPrice `json:"prices"`
}

// priceModel is a model name as agents report it; provider-prefixed variants ("azure/gpt-5.5")
// carry their resellers' prices and are skipped.
var priceModel = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:@-]{0,99}$`)

// FetchPriceList downloads the price list. The file is cached with its ETag, so an unchanged list
// costs a 304. Entries without input and output prices, or with values out of range, are skipped.
func (c *Client) FetchPriceList(ctx context.Context) (*PriceList, error) {
	path := "/repos/" + priceRepo + "/contents/" + priceFile
	body, err := c.cachedRaw(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("ceník %s: %w", PriceListSource, err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("ceník %s: %w", PriceListSource, err)
	}
	list := &PriceList{Prices: map[string]ModelPrice{}}
	for name, entry := range raw {
		if !priceModel.MatchString(name) {
			continue
		}
		var e struct {
			Input      *float64 `json:"input_cost_per_token"`
			Output     *float64 `json:"output_cost_per_token"`
			CacheRead  *float64 `json:"cache_read_input_token_cost"`
			CacheWrite *float64 `json:"cache_creation_input_token_cost"`
		}
		if json.Unmarshal(entry, &e) != nil || e.Input == nil || e.Output == nil {
			continue // documentation entries ("sample_spec") and non-text models
		}
		p := ModelPrice{Input: *e.Input * perMillion, Output: *e.Output * perMillion}
		if e.CacheRead != nil {
			p.CacheRead = *e.CacheRead * perMillion
		}
		if e.CacheWrite != nil {
			p.CacheWrite = *e.CacheWrite * perMillion
		}
		if validPrice(p.Input) && validPrice(p.Output) && validPrice(p.CacheRead) && validPrice(p.CacheWrite) {
			list.Prices[name] = p
		}
	}
	if len(list.Prices) == 0 {
		return nil, fmt.Errorf("ceník %s neobsahuje žádné ceny", PriceListSource)
	}
	list.Date, err = c.fileDate(ctx)
	if err != nil {
		return nil, err
	}
	return list, nil
}

func validPrice(v float64) bool { return v >= 0 && v <= maxPrice && !math.IsNaN(v) }

// cachedRaw fetches raw file contents, replaying the cached body on 304.
func (c *Client) cachedRaw(ctx context.Context, path string) ([]byte, error) {
	var cached Cached
	have := false
	if c.cache != nil {
		cached, have = c.cache.Get(ctx, path)
	}
	etag := ""
	if have {
		etag = cached.ETag
	}
	body, hdr, err := c.doAccept(ctx, http.MethodGet, path, nil, etag, acceptRaw)
	switch {
	case err != nil:
		return nil, err
	case body == nil && have:
		c.cacheHits.Add(1)
		return cached.Body, nil
	case body == nil:
		return nil, fmt.Errorf("%s: 304 bez uložené odpovědi", path)
	}
	if c.cache != nil && hdr.Get("ETag") != "" {
		if err := c.cache.Put(ctx, path, Cached{ETag: hdr.Get("ETag"), Body: body}); err != nil {
			return nil, fmt.Errorf("cache %s: %w", path, err)
		}
	}
	return body, nil
}

// fileDate is the date of the latest commit that changed the price file.
func (c *Client) fileDate(ctx context.Context) (string, error) {
	var commits []struct {
		Commit struct {
			Committer struct {
				Date time.Time `json:"date"`
			} `json:"committer"`
		} `json:"commit"`
	}
	path := "/repos/" + priceRepo + "/commits?path=" + priceFile + "&per_page=1"
	if _, err := c.GetJSON(ctx, path, &commits); err != nil {
		return "", fmt.Errorf("datum ceníku: %w", err)
	}
	if len(commits) == 0 {
		return "", fmt.Errorf("datum ceníku: soubor nemá historii")
	}
	return commits[0].Commit.Committer.Date.UTC().Format("2006-01-02"), nil
}
