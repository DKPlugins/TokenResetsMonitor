package updates

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DKPlugins/TokenResetsMonitor/internal/buildinfo"
)

func fixtureChecker(t *testing.T, handler http.HandlerFunc, prereleases bool) *Checker {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	checker := New("1.1.0", prereleases)
	checker.apiBase = server.URL
	checker.downloadBase = server.URL
	checker.client = server.Client()
	checker.client.CheckRedirect = safeRedirect
	return checker
}

func TestSemanticVersionPrecedence(t *testing.T) {
	for _, test := range []struct {
		a, b string
		want int
	}{
		{"1.10.0", "1.9.0", 1}, {"v1.2.0", "1.2.0+build.7", 0},
		{"1.2.0-rc.10", "1.2.0-rc.2", 1}, {"1.2.0-rc.1", "1.2.0", -1},
		{"1.2.0-alpha.1", "1.2.0-alpha.beta", -1}, {"1.2.0-alpha", "1.2.0-alpha.1", -1},
		{"999999999999999999999.0.0", "99.0.0", 1},
	} {
		got, err := Compare(test.a, test.b)
		if err != nil || got != test.want {
			t.Errorf("%s vs %s: got %d,%v", test.a, test.b, got, err)
		}
	}
	for _, invalid := range []string{"1.01.0", "1.2", "1.2.0-01", "development", "1.2.3-", "1.2.3+", "1.2.3/other"} {
		if _, err := Compare(invalid, "1.0.0"); err == nil {
			t.Errorf("accepted %q", invalid)
		}
	}
}

func TestReleaseAndManifestUseConditionalCache(t *testing.T) {
	var requests, conditional atomic.Int32
	checker := fixtureChecker(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("If-None-Match") == `"fixture"` {
			conditional.Add(1)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"fixture"`)
		if r.URL.Path == "/releases/latest" {
			writeFixtureRelease(w, "v1.2.0", fixturePlatformArchive("v1.2.0"))
			return
		}
		if r.URL.Path != "/v1.2.0/compatibility.json" {
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(404)
			return
		}
		manifest := buildinfo.CurrentManifest()
		manifest.Version = "1.2.0"
		json.NewEncoder(w).Encode(manifest)
	}, false)
	for i := 0; i < 2; i++ {
		result, err := checker.Check(context.Background())
		if err != nil || !result.UpdateAvailable || result.Compatibility != "supported" {
			t.Fatalf("result %+v, %v", result, err)
		}
	}
	if requests.Load() != 4 || conditional.Load() != 2 {
		t.Fatalf("requests=%d conditional=%d", requests.Load(), conditional.Load())
	}
}

func TestPrereleaseSelectionIgnoresDraftsAndUsesSemver(t *testing.T) {
	checker := fixtureChecker(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/releases" || r.URL.Query().Get("per_page") != "100" {
			t.Errorf("unexpected request %s", r.URL)
		}
		io.WriteString(w, `[{"tag_name":"v9.0.0","draft":true},{"tag_name":"v1.3.0-rc.2","prerelease":true},{"tag_name":"v1.3.0-rc.10","prerelease":true},{"tag_name":"garbage"}]`)
	}, true)
	result, err := checker.Check(context.Background())
	if err != nil || result.LatestVersion != "1.3.0-rc.10" || result.Compatibility != "unknown" {
		t.Fatalf("result %+v, %v", result, err)
	}
}

func TestRateLimitStopsRepeatedNetworkRequests(t *testing.T) {
	var calls atomic.Int32
	checker := fixtureChecker(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(429)
	}, false)
	for i := 0; i < 2; i++ {
		if _, err := checker.Check(context.Background()); err == nil {
			t.Fatal("expected rate limit error")
		}
	}
	if calls.Load() != 1 {
		t.Fatal("rate limited checker retried the server")
	}
	if time.Until(checker.notBefore) < time.Minute {
		t.Fatal("Retry-After was ignored")
	}
}

func TestMalformedOversizedAndUnsupportedReleaseResponses(t *testing.T) {
	for _, test := range []struct {
		name, body string
		status     int
	}{
		{"invalid", "not-json", 200}, {"oversize", strings.Repeat("x", maxResponseBytes+1), 200},
		{"notfound", "", 404}, {"stable_rejects_prerelease", `{"tag_name":"v2.0.0-rc.1","prerelease":true}`, 200},
		{"cache_missing", "", 304},
	} {
		t.Run(test.name, func(t *testing.T) {
			checker := fixtureChecker(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(test.status); io.WriteString(w, test.body) }, false)
			if _, err := checker.Check(context.Background()); err == nil {
				t.Fatal("expected safe error")
			}
		})
	}
}

func TestCanceledCheckAndHTTPSOnly(t *testing.T) {
	checker := New("1.0.0", false)
	checker.apiBase = "http://127.0.0.1:1"
	if _, err := checker.Check(context.Background()); err == nil || !strings.Contains(err.Error(), "HTTPS") {
		t.Fatalf("expected HTTPS rejection: %v", err)
	}
	checker = fixtureChecker(t, func(w http.ResponseWriter, r *http.Request) { t.Error("canceled check reached server") }, false)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := checker.Check(ctx); err == nil {
		t.Fatal("expected cancellation")
	}
}

func TestReleaseRedirectPolicy(t *testing.T) {
	for _, destination := range []string{"http://github.com/example", "https://attacker.example/path", "https://user:secret@github.com/a"} {
		req, _ := http.NewRequest(http.MethodGet, destination, nil)
		if safeRedirect(req, nil) == nil {
			t.Fatalf("accepted %s", destination)
		}
	}
}

func TestCompatibilityIsExplicitAndDoesNotInferFromVersion(t *testing.T) {
	current := buildinfo.CurrentManifest()
	target := current
	target.State.Minimum = current.State.Current + 1
	target.State.Current = target.State.Minimum
	if status, _ := Compatibility(target, current, "linux/amd64"); status != "incompatible" {
		t.Fatal("accepted unreadable source schema")
	}
	target = current
	if status, _ := Compatibility(target, current, "darwin/arm64"); status != "incompatible" {
		t.Fatal("accepted unavailable binary")
	}
	target = current
	target.ManifestVersion = 2
	if validManifest(target, target.Version) {
		t.Fatal("accepted unknown manifest format")
	}
}

func fixturePlatformArchive(tag string) string {
	suffix := ".tar.gz"
	if runtime.GOOS == "windows" {
		suffix = ".zip"
	}
	return fmt.Sprintf("tokenresetsmonitor_%s_%s_%s%s", tag, runtime.GOOS, runtime.GOARCH, suffix)
}
func writeFixtureRelease(w http.ResponseWriter, tag string, archives ...string) {
	assets := []map[string]string{{"name": "compatibility.json"}}
	for _, name := range archives {
		assets = append(assets, map[string]string{"name": name})
	}
	json.NewEncoder(w).Encode(map[string]any{"tag_name": tag, "assets": assets})
}

func TestCompatibilityDoesNotAssumeLegacyFilesWereMigrated(t *testing.T) {
	current := buildinfo.CurrentManifest()
	for _, schema := range []string{"configuration", "state"} {
		t.Run(schema, func(t *testing.T) {
			target := current
			if schema == "configuration" {
				target.Config.Minimum = current.Config.Current
			} else {
				target.State.Minimum = current.State.Current
			}
			status, notes := Compatibility(target, current, runtime.GOOS+"/"+runtime.GOARCH)
			if status != "unknown" || len(notes) == 0 || !strings.Contains(strings.Join(notes, " "), "legacy") {
				t.Fatalf("legacy source incorrectly declared supported: %s %v", status, notes)
			}
			// A caller with verified exact source versions can narrow both ranges.
			actual := current
			actual.Config.Minimum = actual.Config.Current
			actual.State.Minimum = actual.State.Current
			if status, notes := Compatibility(target, actual, runtime.GOOS+"/"+runtime.GOARCH); status != "supported" {
				t.Fatalf("verified current schemas rejected: %s %v", status, notes)
			}
		})
	}
}

func TestReleaseRequiresTheActualPlatformArchive(t *testing.T) {
	for _, tc := range []struct{ name, archive, want string }{
		{"missing", "", "incompatible"},
		{"wrong_version", fixturePlatformArchive("v1.1.0"), "incompatible"},
		{"wrong_platform", "tokenresetsmonitor_v1.2.0_unavailable_unknown.tar.gz", "incompatible"},
		{"present", fixturePlatformArchive("v1.2.0"), "supported"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checker := fixtureChecker(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/releases/latest" {
					writeFixtureRelease(w, "v1.2.0", tc.archive)
					return
				}
				manifest := buildinfo.CurrentManifest()
				manifest.Version = "1.2.0"
				json.NewEncoder(w).Encode(manifest)
			}, false)
			result, err := checker.Check(context.Background())
			if err != nil || result.Compatibility != tc.want || !result.UpdateAvailable {
				t.Fatalf("result %+v, %v", result, err)
			}
			if tc.want == "incompatible" && !strings.Contains(strings.Join(result.Notes, " "), "expected archive") {
				t.Fatal("missing archive not explained")
			}
		})
	}
}

func TestReleaseWithDroppedLegacySchemaIsUnconfirmed(t *testing.T) {
	checker := fixtureChecker(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/releases/latest" {
			writeFixtureRelease(w, "v1.2.0", fixturePlatformArchive("v1.2.0"))
			return
		}
		manifest := buildinfo.CurrentManifest()
		manifest.Version = "1.2.0"
		manifest.Config.Minimum = manifest.Config.Current
		json.NewEncoder(w).Encode(manifest)
	}, false)
	result, err := checker.Check(context.Background())
	if err != nil || result.Compatibility != "unknown" || !result.UpdateAvailable {
		t.Fatalf("result %+v, %v", result, err)
	}
}

func TestGitHubHTTPDateRateLimitIsHonored(t *testing.T) {
	var calls atomic.Int32
	deadline := time.Now().Add(3 * time.Minute).UTC().Truncate(time.Second)
	checker := fixtureChecker(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", deadline.Format(http.TimeFormat))
		w.WriteHeader(http.StatusTooManyRequests)
	}, false)
	if _, err := checker.Check(context.Background()); err == nil {
		t.Fatal("expected rate limit")
	}
	if checker.notBefore.Before(deadline.Add(-time.Second)) || checker.notBefore.After(deadline.Add(time.Second)) {
		t.Fatalf("HTTP-date delay lost: %s want %s", checker.notBefore, deadline)
	}
	if _, err := checker.Check(context.Background()); err == nil || calls.Load() != 1 {
		t.Fatal("checker repeated request before HTTP-date deadline")
	}
}
