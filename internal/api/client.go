// Package api reads the public TokenResets API. It never infers account state.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/DKPlugins/TokenResetsMonitor/internal/model"
)

const maxResponseBytes = 8 << 20
const maxPages = 1000

var identifier = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,255}$`)
var schemaVersion = regexp.MustCompile(`^1\.[0-9]+$`)

type Cache interface {
	GetCache(string) (model.CachedResponse, bool, error)
	PutCache(string, model.CachedResponse) error
}

// Error contains only safe diagnostic text, never a URL, response body, or token.
type Error struct {
	StatusCode int
	RetryAfter time.Duration
	Retryable  bool
	Message    string
}

func (e *Error) Error() string { return e.Message }

type Client struct {
	base  string
	http  *http.Client
	cache Cache
}

func New(baseURL string, timeout time.Duration, cache Cache) *Client {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &Client{base: strings.TrimRight(baseURL, "/"), cache: cache, http: &http.Client{
		Timeout:       timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

type envelope struct {
	Data       json.RawMessage `json:"data"`
	Pagination *struct {
		NextCursor *string `json:"next_cursor"`
		HasMore    bool    `json:"has_more"`
	} `json:"pagination"`
	Meta struct {
		SchemaVersion string `json:"schema_version"`
	} `json:"meta"`
}

func decodeEnvelope(body []byte) (envelope, error) {
	var e envelope
	if json.Unmarshal(body, &e) != nil {
		return e, errors.New("API returned invalid JSON")
	}
	if !schemaVersion.MatchString(e.Meta.SchemaVersion) {
		return e, errors.New("API returned an unsupported schema version")
	}
	if len(e.Data) == 0 || string(e.Data) == "null" {
		return e, errors.New("API response is missing data")
	}
	return e, nil
}

func (c *Client) get(ctx context.Context, path string) (envelope, error) {
	var empty envelope
	endpoint := c.base + path
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return empty, errors.New("API base URL is invalid")
	}
	var cached model.CachedResponse
	if c.cache != nil {
		var found bool
		cached, found, err = c.cache.GetCache(endpoint)
		if err != nil {
			return empty, errors.New("cannot read API response cache")
		}
		if !found {
			cached = model.CachedResponse{}
		}
	}
	for attempt := 0; attempt < 2; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return empty, errors.New("cannot create API request")
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", "TokenResetsMonitor/1")
		if attempt == 0 && cached.ETag != "" {
			req.Header.Set("If-None-Match", cached.ETag)
		}
		res, err := c.http.Do(req)
		if err != nil {
			return empty, networkError(ctx, err)
		}
		if res.StatusCode == http.StatusNotModified {
			res.Body.Close()
			if len(cached.Body) != 0 && attempt == 0 {
				if e, err := decodeEnvelope(cached.Body); err == nil {
					return e, nil
				}
			}
			continue // Cache body missing/corrupt: retry once without If-None-Match.
		}
		if res.StatusCode < 200 || res.StatusCode >= 300 {
			res.Body.Close()
			return empty, &Error{StatusCode: res.StatusCode, Retryable: res.StatusCode == 429 || res.StatusCode >= 500,
				RetryAfter: ParseRetryAfter(res.Header.Get("Retry-After"), time.Now()), Message: fmt.Sprintf("API returned HTTP %d", res.StatusCode)}
		}
		body, readErr := io.ReadAll(io.LimitReader(res.Body, maxResponseBytes+1))
		res.Body.Close()
		if readErr != nil {
			return empty, &Error{Retryable: true, Message: "cannot read API response"}
		}
		if len(body) > maxResponseBytes {
			return empty, errors.New("API response exceeds size limit")
		}
		e, err := decodeEnvelope(body)
		if err != nil {
			return empty, err
		}
		if c.cache != nil {
			if err := c.cache.PutCache(endpoint, model.CachedResponse{ETag: res.Header.Get("ETag"), Body: body}); err != nil {
				return empty, errors.New("cannot save API response cache")
			}
		}
		return e, nil
	}
	return empty, errors.New("API returned 304 without a usable cached response")
}

func networkError(ctx context.Context, err error) error {
	if errors.Is(ctx.Err(), context.Canceled) {
		return &Error{Message: "API request canceled"}
	}
	var netErr net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()) {
		return &Error{Retryable: true, Message: "API request timed out"}
	}
	return &Error{Retryable: true, Message: "API network request failed"}
}

// ParseRetryAfter supports both HTTP delta-seconds and an HTTP date. Durations
// are bounded to seven days to avoid overflow or unbounded source-controlled waits.
func ParseRetryAfter(value string, now time.Time) time.Duration {
	const maxWait = 7 * 24 * time.Hour
	if seconds, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64); err == nil {
		if seconds <= 0 {
			return 0
		}
		if seconds > int64(maxWait/time.Second) {
			return maxWait
		}
		return time.Duration(seconds) * time.Second
	}
	if date, err := http.ParseTime(value); err == nil {
		d := date.Sub(now)
		if d < 0 {
			return 0
		}
		if d > maxWait {
			return maxWait
		}
		return d
	}
	return 0
}

func (c *Client) Catalog(ctx context.Context) ([]model.Provider, error) {
	e, err := c.get(ctx, "/providers")
	if err != nil {
		return nil, err
	}
	var providers []model.Provider
	if json.Unmarshal(e.Data, &providers) != nil {
		return nil, errors.New("API provider catalog is invalid")
	}
	seen := make(map[string]bool)
	for _, p := range providers {
		if !identifier.MatchString(p.Slug) || p.Name == "" || seen[p.Slug] {
			return nil, errors.New("API provider catalog contains an invalid provider")
		}
		seen[p.Slug] = true
	}
	return providers, nil
}

func (c *Client) Detail(ctx context.Context, slug string) (model.Provider, error) {
	var provider model.Provider
	if !identifier.MatchString(slug) {
		return provider, errors.New("invalid provider identifier")
	}
	e, err := c.get(ctx, "/providers/"+slug)
	if err != nil {
		return provider, err
	}
	if json.Unmarshal(e.Data, &provider) != nil || provider.Slug != slug || provider.Name == "" {
		return model.Provider{}, errors.New("API provider detail does not match requested provider")
	}
	for _, values := range [][]model.NamedValue{provider.Products, provider.Plans, provider.Windows} {
		seen := make(map[string]bool)
		for _, v := range values {
			if !identifier.MatchString(v.Slug) || seen[v.Slug] {
				return model.Provider{}, errors.New("API provider detail contains an invalid scope value")
			}
			seen[v.Slug] = true
		}
	}
	return provider, nil
}

// Events reads the entire history even when the first page is unchanged. The
// caller must commit the resulting snapshot only after this method succeeds.
func (c *Client) Events(ctx context.Context, slug string) ([]model.Event, error) {
	if !identifier.MatchString(slug) {
		return nil, errors.New("invalid provider identifier")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	var events []model.Event
	seenCursor := make(map[string]bool)
	seenEvents := make(map[string]bool)
	cursor := ""
	for page := 0; page < maxPages; page++ {
		query := url.Values{"limit": {"100"}}
		if cursor != "" {
			query.Set("cursor", cursor)
		}
		e, err := c.get(ctx, "/providers/"+slug+"/events?"+query.Encode())
		if err != nil {
			return nil, err
		}
		if e.Pagination == nil {
			return nil, errors.New("API event response is missing pagination")
		}
		var batch []model.Event
		if json.Unmarshal(e.Data, &batch) != nil {
			return nil, errors.New("API event data is invalid")
		}
		if len(batch) > 100 {
			return nil, errors.New("API page exceeds the requested event limit")
		}
		for i := range batch {
			if err := c.validateEvent(&batch[i]); err != nil {
				return nil, err
			}
			if batch[i].Provider.Slug != slug {
				return nil, errors.New("API event does not match requested provider")
			}
			if seenEvents[batch[i].ID] {
				return nil, errors.New("API pagination returned a duplicate event; retry a fresh scan")
			}
			seenEvents[batch[i].ID] = true
			events = append(events, batch[i])
		}
		if !e.Pagination.HasMore {
			return events, nil
		}
		if len(batch) == 0 || e.Pagination.NextCursor == nil || *e.Pagination.NextCursor == "" {
			return nil, errors.New("API pagination is incomplete")
		}
		cursor = *e.Pagination.NextCursor
		if len(cursor) > 4096 || seenCursor[cursor] {
			return nil, errors.New("API pagination cursor is invalid or repeated")
		}
		seenCursor[cursor] = true
	}
	return nil, errors.New("API history exceeds page limit")
}

func (c *Client) Event(ctx context.Context, id string) (model.Event, error) {
	var event model.Event
	if !identifier.MatchString(id) {
		return event, errors.New("invalid event identifier")
	}
	e, err := c.get(ctx, "/events/"+id)
	if err != nil {
		return event, err
	}
	if json.Unmarshal(e.Data, &event) != nil {
		return model.Event{}, errors.New("API event detail is invalid")
	}
	if err := c.validateEvent(&event); err != nil {
		return model.Event{}, err
	}
	if event.ID != id && event.Slug != id {
		return model.Event{}, errors.New("API event does not match requested identifier")
	}
	return event, nil
}

func (c *Client) validateEvent(event *model.Event) error {
	if !identifier.MatchString(event.ID) || !identifier.MatchString(event.Provider.Slug) || event.Revision < 1 || event.EventType == "" || event.Status == "" || event.AnnouncedAt.IsZero() {
		return errors.New("API event is missing valid required fields")
	}
	base, _ := url.Parse(c.base)
	for _, link := range []*string{&event.Links.HTML, &event.Links.Self} {
		if *link == "" {
			continue
		}
		u, err := url.Parse(*link)
		if err != nil {
			return errors.New("API event has an invalid link")
		}
		u = base.ResolveReference(u)
		if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
			return errors.New("API event has an invalid link")
		}
		*link = u.String()
	}
	return nil
}
