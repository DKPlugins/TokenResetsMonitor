package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestPrivateControlRoundTripAndCleanup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	var calls atomic.Int32
	server, err := Start(path, func(ctx context.Context, r Request) (any, error) {
		calls.Add(1)
		if r.Operation != "history.list" {
			t.Error("wrong operation")
		}
		return WithMetadata(map[string]string{"result": "okay"}, "running", 7)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	d, err := readDescriptor(DescriptorPath(path))
	if err != nil || len(d.Token) != 64 {
		t.Fatalf("descriptor: %v", err)
	}
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	var result Result
	if err := Call(context.Background(), path, Request{Operation: "history.list"}, &result); err != nil {
		t.Fatal(err)
	}
	if result.ConfigGeneration != 7 || result.ConfigurationSource != "running" || !strings.Contains(string(result.Data), "okay") || calls.Load() != 1 {
		t.Fatalf("result: %+v", result)
	}
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(DescriptorPath(path)); !os.IsNotExist(err) {
		t.Fatal("descriptor was not removed")
	}
	if err := Call(context.Background(), path, Request{Operation: "history.list"}, &result); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("missing endpoint: %v", err)
	}
}

func TestLocalControlAuthenticationAndRequestBounds(t *testing.T) {
	const token = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	const host = "127.0.0.1:12345"
	var calls int
	handler := localHandler(token, host, func(context.Context, Request) (any, error) { calls++; return map[string]bool{"ok": true}, nil })
	tests := []struct {
		name, body, auth, origin, host string
		status                         int
	}{
		{"no_auth", `{"operation":"history.list","query":{}}`, "", "", host, http.StatusUnauthorized},
		{"wrong_auth", `{"operation":"history.list","query":{}}`, "Bearer wrong", "", host, http.StatusUnauthorized},
		{"browser_origin", `{"operation":"history.list","query":{}}`, "Bearer " + token, "https://example.invalid", host, http.StatusBadRequest},
		{"wrong_host", `{"operation":"history.list","query":{}}`, "Bearer " + token, "", "attacker.invalid", http.StatusBadRequest},
		{"unknown_field", `{"operation":"history.list","query":{},"surprise":true}`, "Bearer " + token, "", host, http.StatusBadRequest},
		{"second_json", `{"operation":"history.list","query":{}} {}`, "Bearer " + token, "", host, http.StatusBadRequest},
		{"oversized", strings.Repeat(" ", maxRequestBytes+1), "Bearer " + token, "", host, http.StatusBadRequest},
		{"valid", `{"operation":"history.list","query":{}}`, "Bearer " + token, "", host, http.StatusOK},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "http://"+host+"/v1/command", strings.NewReader(test.body))
			request.RemoteAddr = "127.0.0.1:45678"
			request.Host = test.host
			request.Header.Set("Authorization", test.auth)
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Origin", test.origin)
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			if recorder.Code != test.status {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
		})
	}
	if calls != 1 {
		t.Fatalf("invalid requests reached dispatcher: %d", calls)
	}
}

func TestLocalControlSanitizesDispatcherFailures(t *testing.T) {
	server, err := Start(filepath.Join(t.TempDir(), "state.db"), func(context.Context, Request) (any, error) { return nil, errors.New("SECRET URL password token") })
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	var value any
	err = Call(context.Background(), strings.TrimSuffix(server.path, ".control.json"), Request{Operation: "history.list"}, &value)
	if err == nil || strings.Contains(err.Error(), "SECRET") || errors.Is(err, ErrUnavailable) {
		t.Fatalf("failure not safely preserved: %v", err)
	}
}

func TestClientRejectsRemoteDescriptorAndRedirects(t *testing.T) {
	for _, endpoint := range []string{"https://127.0.0.1:123/v1/command", "http://localhost:123/v1/command", "http://example.com:123/v1/command", "http://127.0.0.1:123/v1/command?token=secret", "http://127.0.0.1:123/v1/command#x", "http://127.0.0.1:0123/v1/command", "http://user@127.0.0.1:123/v1/command", "http://127.0.0.1:123/other"} {
		if validEndpoint(endpoint) == nil {
			t.Fatalf("unsafe endpoint accepted: %s", endpoint)
		}
	}
	var redirected atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected.Add(1); w.WriteHeader(http.StatusNoContent) }))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	path := filepath.Join(t.TempDir(), "state.db")
	if err := writeDescriptor(DescriptorPath(path), descriptor{Version: protocolVersion, Endpoint: source.URL + "/v1/command", Token: strings.Repeat("a", 64), PID: 1}); err != nil {
		t.Fatal(err)
	}
	var result any
	if err := Call(context.Background(), path, Request{Operation: "history.list"}, &result); err == nil || errors.Is(err, ErrUnavailable) {
		t.Fatalf("redirect accepted: %v", err)
	}
	if redirected.Load() != 0 {
		t.Fatal("redirect forwarded local credentials")
	}
}

func TestControlDoesNotFallbackAfterAmbiguousTimeout(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	server, err := Start(path, func(ctx context.Context, r Request) (any, error) { <-ctx.Done(); return nil, ctx.Err() })
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	var result any
	if err := Call(ctx, path, Request{Operation: "history.list"}, &result); err == nil || errors.Is(err, ErrUnavailable) {
		t.Fatalf("ambiguous request incorrectly allowed fallback: %v", err)
	}
}

func TestResponseSizeBound(t *testing.T) {
	recorder := httptest.NewRecorder()
	writeResponse(recorder, strings.Repeat("x", maxResponseBytes), nil)
	var result response
	if json.Unmarshal(recorder.Body.Bytes(), &result) != nil || result.Error != "response_too_large" || recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatal("oversized response was emitted")
	}
}

func TestStaleDescriptorAllowsOfflineFallback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	server, err := Start(path, func(context.Context, Request) (any, error) { return map[string]bool{"okay": true}, nil })
	if err != nil {
		t.Fatal(err)
	}
	d, err := readDescriptor(DescriptorPath(path))
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	if err := writeDescriptor(DescriptorPath(path), d); err != nil {
		t.Fatal(err)
	}
	var result any
	if err := Call(context.Background(), path, Request{Operation: "history.list"}, &result); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("closed loopback endpoint should permit offline lock attempt: %v", err)
	}
}
