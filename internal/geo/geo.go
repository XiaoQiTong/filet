package geo

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Resolver maps public IPs to a human-readable location through an
// ip-api.com compatible endpoint. Results (including failures) are cached so a
// burst of reconnects does not hammer the upstream service.
type Resolver struct {
	template string
	client   *http.Client
	mu       sync.Mutex
	cache    map[string]entry
}

type entry struct {
	location string
	expires  time.Time
}

func NewResolver(template string) *Resolver {
	template = strings.TrimSpace(template)
	if template == "" {
		return nil
	}
	return &Resolver{
		template: template,
		client:   &http.Client{Timeout: 4 * time.Second},
		cache:    map[string]entry{},
	}
}

func (r *Resolver) Lookup(ctx context.Context, ip string) string {
	if r == nil || ip == "" {
		return ""
	}
	now := time.Now()
	r.mu.Lock()
	if e, ok := r.cache[ip]; ok && now.Before(e.expires) {
		r.mu.Unlock()
		return e.location
	}
	r.mu.Unlock()

	location := r.fetch(ctx, ip)
	ttl := 10 * time.Minute
	if location != "" {
		ttl = 24 * time.Hour
	}
	r.mu.Lock()
	if len(r.cache) >= 4096 {
		for k, e := range r.cache {
			if now.After(e.expires) {
				delete(r.cache, k)
			}
		}
	}
	r.cache[ip] = entry{location: location, expires: now.Add(ttl)}
	r.mu.Unlock()
	return location
}

func (r *Resolver) fetch(ctx context.Context, ip string) string {
	endpoint := strings.ReplaceAll(r.template, "{ip}", url.QueryEscape(ip))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("User-Agent", "filet")
	resp, err := r.client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	var body struct {
		Status     string `json:"status"`
		Country    string `json:"country"`
		RegionName string `json:"regionName"`
		City       string `json:"city"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&body); err != nil {
		return ""
	}
	if body.Status != "" && !strings.EqualFold(body.Status, "success") {
		return ""
	}
	parts := make([]string, 0, 3)
	for _, p := range []string{body.Country, body.RegionName, body.City} {
		p = strings.TrimSpace(p)
		if p == "" || (len(parts) > 0 && parts[len(parts)-1] == p) {
			continue
		}
		parts = append(parts, p)
	}
	return strings.Join(parts, " ")
}
