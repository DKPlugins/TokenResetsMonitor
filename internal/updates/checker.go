package updates

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/DKPlugins/TokenResetsMonitor/internal/api"
	"github.com/DKPlugins/TokenResetsMonitor/internal/buildinfo"
)

const repository = "DKPlugins/TokenResetsMonitor"
const maxResponseBytes = 2 << 20

type Result struct {
	CheckedAt       time.Time `json:"checked_at"`
	CurrentVersion  string    `json:"current_version"`
	LatestVersion   string    `json:"latest_version,omitempty"`
	ReleaseURL      string    `json:"release_url,omitempty"`
	UpdateAvailable bool      `json:"update_available"`
	Compatibility   string    `json:"compatibility"`
	Notes           []string  `json:"notes,omitempty"`
}

type cachedResponse struct {
	etag string
	body []byte
}
type release struct {
	Tag        string `json:"tag_name"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
	Assets     []struct {
		Name string `json:"name"`
	} `json:"assets"`
}

// Checker retains bounded conditional-response caches and rate-limit deadlines.
// It is safe to share between a manual caller and the background scheduler.
type Checker struct {
	mu                sync.Mutex
	current           string
	includePrerelease bool
	client            *http.Client
	apiBase           string
	downloadBase      string
	cache             map[string]cachedResponse
	notBefore         time.Time
}

func New(currentVersion string, includePrerelease bool) *Checker {
	return &Checker{current: currentVersion, includePrerelease: includePrerelease,
		apiBase:      "https://api.github.com/repos/" + repository,
		downloadBase: "https://github.com/" + repository + "/releases/download",
		cache:        make(map[string]cachedResponse), client: &http.Client{Timeout: 15 * time.Second, CheckRedirect: safeRedirect}}
}

func safeRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 5 || req.URL.Scheme != "https" || req.URL.User != nil {
		return errors.New("unsafe release redirect")
	}
	switch req.URL.Hostname() {
	case "github.com", "api.github.com", "release-assets.githubusercontent.com", "objects.githubusercontent.com":
		return nil
	}
	return errors.New("unexpected release redirect host")
}

func (c *Checker) Check(ctx context.Context) (Result, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	result := Result{CheckedAt: time.Now().UTC(), CurrentVersion: c.current, Compatibility: "unknown"}
	if _, err := parseVersion(c.current); err != nil {
		return result, errors.New("current build has no comparable semantic version")
	}
	if time.Now().Before(c.notBefore) {
		return result, errors.New("release check is waiting for GitHub rate-limit recovery")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	path := c.apiBase + "/releases/latest"
	if c.includePrerelease {
		path = c.apiBase + "/releases?per_page=100"
	}
	body, status, err := c.get(ctx, path)
	if err != nil {
		return result, err
	}
	if status == http.StatusNotFound {
		return result, errors.New("no published release was found")
	}
	var candidates []release
	if c.includePrerelease {
		if json.Unmarshal(body, &candidates) != nil {
			return result, errors.New("GitHub returned invalid release metadata")
		}
	} else {
		var candidate release
		if json.Unmarshal(body, &candidate) != nil {
			return result, errors.New("GitHub returned invalid release metadata")
		}
		candidates = []release{candidate}
	}
	var latest *release
	for i := range candidates {
		r := &candidates[i]
		parsed, e := parseVersion(r.Tag)
		if e != nil || !strings.HasPrefix(r.Tag, "v") || r.Draft || (!c.includePrerelease && (r.Prerelease || parsed[4] != "")) {
			continue
		}
		if latest == nil {
			latest = r
			continue
		}
		if cmp, _ := Compare(r.Tag, latest.Tag); cmp > 0 {
			latest = r
		}
	}
	if latest == nil {
		return result, errors.New("no valid release exists in the selected channel")
	}
	result.LatestVersion = strings.TrimPrefix(latest.Tag, "v")
	result.ReleaseURL = "https://github.com/" + repository + "/releases/tag/" + url.PathEscape(latest.Tag)
	cmp, _ := Compare(latest.Tag, c.current)
	result.UpdateAvailable = cmp > 0
	hasManifest, hasArchive := false, false
	archive := releaseArchiveName(latest.Tag, runtime.GOOS, runtime.GOARCH)
	for _, asset := range latest.Assets {
		if asset.Name == "compatibility.json" {
			hasManifest = true
		}
		if asset.Name == archive {
			hasArchive = true
		}
	}
	if !hasManifest {
		result.Notes = []string{"This release does not publish compatibility metadata; inspect its release notes."}
		return result, nil
	}
	body, status, err = c.get(ctx, c.downloadBase+"/"+url.PathEscape(latest.Tag)+"/compatibility.json")
	if err != nil {
		result.Notes = []string{"Compatibility metadata could not be checked."}
		return result, err
	}
	if status == http.StatusNotFound {
		result.Notes = []string{"Compatibility metadata is unavailable."}
		return result, nil
	}
	var manifest buildinfo.Manifest
	if json.Unmarshal(body, &manifest) != nil || !validManifest(manifest, result.LatestVersion) {
		result.Notes = []string{"The release compatibility manifest is invalid or unsupported."}
		return result, errors.New("invalid release compatibility manifest")
	}
	result.Compatibility, result.Notes = Compatibility(manifest, buildinfo.CurrentManifest(), runtime.GOOS+"/"+runtime.GOARCH)
	if !hasArchive {
		result.Compatibility = "incompatible"
		result.Notes = append(result.Notes, "The release does not contain the expected archive for this platform: "+archive+".")
	}
	return result, nil
}

func validManifest(m buildinfo.Manifest, version string) bool {
	return m.ManifestVersion == 1 && m.Version == version && m.Config.Minimum >= 0 && m.Config.Current >= m.Config.Minimum &&
		m.State.Minimum >= 0 && m.State.Current >= m.State.Minimum && m.WebhookSchema > 0 && m.APISchemaMajor > 0 && len(m.Platforms) > 0 && len(m.Platforms) <= 32
}

// Compatibility compares declared schema ranges and platform support. Since a
// running binary can still be using a legacy config/state file, a target that
// drops part of the current accepted range is unconfirmed, even when it reads
// the schema this binary writes. No live files are opened or migrated here.
func Compatibility(target, current buildinfo.Manifest, platform string) (string, []string) {
	var notes []string
	if current.Config.Current < target.Config.Minimum || current.Config.Current > target.Config.Current {
		notes = append(notes, "The release cannot read this binary's current configuration schema.")
	}
	if current.State.Current < target.State.Minimum || current.State.Current > target.State.Current {
		notes = append(notes, "The release cannot read this binary's current state schema.")
	}
	if target.WebhookSchema != current.WebhookSchema {
		notes = append(notes, "The webhook payload schema changes; review receivers before updating.")
	}
	if target.APISchemaMajor != current.APISchemaMajor {
		notes = append(notes, "The source API contract changes; inspect the release notes.")
	}
	found := false
	for _, p := range target.Platforms {
		if p == platform {
			found = true
		}
	}
	if !found {
		notes = append(notes, "No release binary is declared for this platform.")
	}
	if len(notes) > 0 {
		return "incompatible", notes
	}
	if current.Config.Minimum < target.Config.Minimum {
		notes = append(notes, "This release drops legacy configuration schemas accepted by the current binary. Check the saved config_version and migrate it with the current binary if needed before installing.")
	}
	if current.State.Minimum < target.State.Minimum {
		notes = append(notes, "This release drops legacy state schemas accepted by the current binary. Verify the database schema and the release migration instructions before installing.")
	}
	if len(notes) > 0 {
		return "unknown", notes
	}
	return "supported", []string{"The release declares support for every configuration/state schema accepted by this binary and the current platform. Back up state and review release notes before installing."}
}

func (c *Checker) get(ctx context.Context, endpoint string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, 0, errors.New("cannot create release check")
	}
	if req.URL.Scheme != "https" || req.URL.User != nil {
		return nil, 0, errors.New("release checks require HTTPS")
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2026-03-10")
	req.Header.Set("User-Agent", "TokenResetsMonitor/"+c.current)
	if old, ok := c.cache[endpoint]; ok && old.etag != "" {
		req.Header.Set("If-None-Match", old.etag)
	}
	response, err := c.client.Do(req)
	if err != nil {
		return nil, 0, errors.New("release check failed; check connectivity and proxy settings")
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotModified {
		if old, ok := c.cache[endpoint]; ok {
			return old.body, http.StatusOK, nil
		}
		return nil, 0, errors.New("release server returned 304 without cached metadata")
	}
	if response.StatusCode == http.StatusNotFound {
		return nil, response.StatusCode, nil
	}
	if response.StatusCode == http.StatusForbidden || response.StatusCode == http.StatusTooManyRequests {
		wait := time.Minute
		if delay := api.ParseRetryAfter(response.Header.Get("Retry-After"), time.Now()); delay > 0 {
			wait = delay
		}
		if n, e := strconv.ParseInt(response.Header.Get("X-RateLimit-Reset"), 10, 64); e == nil {
			if d := time.Until(time.Unix(n, 0)); d > wait {
				wait = d
			}
		}
		if wait > 7*24*time.Hour {
			wait = 7 * 24 * time.Hour
		}
		c.notBefore = time.Now().Add(wait)
		return nil, response.StatusCode, errors.New("GitHub release checks are rate limited or access was denied; retry later")
	}
	if response.StatusCode != http.StatusOK {
		return nil, response.StatusCode, fmt.Errorf("release server returned HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil || len(body) > maxResponseBytes {
		return nil, response.StatusCode, errors.New("release metadata is unreadable or exceeds 2 MiB")
	}
	if len(c.cache) >= 8 {
		c.cache = make(map[string]cachedResponse)
	}
	c.cache[endpoint] = cachedResponse{etag: response.Header.Get("ETag"), body: body}
	return body, response.StatusCode, nil
}

// Archive naming is the release/install contract in scripts/release.sh.
func releaseArchiveName(tag, goos, goarch string) string {
	extension := ".tar.gz"
	if goos == "windows" {
		extension = ".zip"
	}
	return "tokenresetsmonitor_" + tag + "_" + goos + "_" + goarch + extension
}
