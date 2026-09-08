// Package control provides the daemon's private, authenticated local command
// endpoint. It is independent of the public observability listener.
package control

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/DKPlugins/TokenResetsMonitor/internal/config"
	"github.com/DKPlugins/TokenResetsMonitor/internal/fileio"
	"github.com/DKPlugins/TokenResetsMonitor/internal/state"
)

const (
	protocolVersion    = 1
	maxRequestBytes    = 64 << 10
	maxResponseBytes   = 8 << 20
	maxDescriptorBytes = 4096
)

var (
	// ErrUnavailable permits an offline database operation. Authentication,
	// timeout and protocol failures deliberately do not permit this fallback.
	ErrUnavailable = errors.New("local control endpoint is unavailable")
	ErrNotFound    = errors.New("requested record does not exist")
	ErrInvalid     = errors.New("invalid management request")
	ErrBusy        = errors.New("configuration is changing; retry the command shortly")
)

type Request struct {
	Operation string             `json:"operation"`
	Provider  string             `json:"provider,omitempty"`
	ID        string             `json:"id,omitempty"`
	Query     state.Query        `json:"query"`
	Candidate *config.FilterSpec `json:"candidate,omitempty"`
}

type descriptor struct {
	Version  int    `json:"version"`
	Endpoint string `json:"endpoint"`
	Token    string `json:"token"`
	PID      int    `json:"pid"`
}

type response struct {
	Data  json.RawMessage `json:"data,omitempty"`
	Error string          `json:"error,omitempty"`
}

type Handler func(context.Context, Request) (any, error)

// Result identifies the configuration used to answer a command; a saved file
// may differ from the generation currently accepted by the running daemon.
type Result struct {
	Data                json.RawMessage `json:"data"`
	ConfigurationSource string          `json:"configuration_source"`
	ConfigGeneration    uint64          `json:"config_generation,omitempty"`
}

func WithMetadata(value any, source string, generation uint64) (Result, error) {
	data, err := json.Marshal(value)
	return Result{Data: data, ConfigurationSource: source, ConfigGeneration: generation}, err
}

type Server struct {
	http       *http.Server
	path       string
	token      string
	errors     chan error
	once       sync.Once
	closeError error
}

func DescriptorPath(statePath string) string { return statePath + ".control.json" }

// Start must be called only while the daemon owns the state database lock.
func Start(statePath string, handler Handler) (*Server, error) {
	if statePath == "" || handler == nil {
		return nil, ErrInvalid
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, errors.New("cannot listen on local control endpoint")
	}
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		listener.Close()
		return nil, errors.New("cannot create local control credentials")
	}
	token := hex.EncodeToString(secret[:])
	s := &Server{path: DescriptorPath(statePath), token: token, errors: make(chan error, 1)}
	s.http = &http.Server{
		Handler:           localHandler(token, listener.Addr().String(), handler),
		ReadHeaderTimeout: 2 * time.Second, ReadTimeout: 5 * time.Second,
		WriteTimeout: 5 * time.Second, IdleTimeout: 5 * time.Second, MaxHeaderBytes: 8 << 10,
	}
	d := descriptor{Version: protocolVersion, Endpoint: "http://" + listener.Addr().String() + "/v1/command", Token: token, PID: os.Getpid()}
	if err := writeDescriptor(s.path, d); err != nil {
		listener.Close()
		return nil, err
	}
	go func() {
		if err := s.http.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.errors <- errors.New("local control server stopped unexpectedly")
		}
		close(s.errors)
	}()
	return s, nil
}

func (s *Server) Errors() <-chan error { return s.errors }

func (s *Server) Close() error {
	s.once.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		s.closeError = s.http.Shutdown(ctx)
		if s.closeError != nil {
			_ = s.http.Close()
		}
		// Never remove a descriptor already replaced by another instance.
		if d, err := readDescriptor(s.path); err == nil && d.Token == s.token {
			if err := os.Remove(s.path); err != nil && !os.IsNotExist(err) {
				s.closeError = errors.Join(s.closeError, errors.New("cannot remove local control descriptor"))
			}
		}
	})
	return s.closeError
}

func writeDescriptor(path string, d descriptor) error {
	data, err := json.Marshal(d)
	if err != nil {
		return errors.New("cannot encode local control descriptor")
	}
	// A unique private temporary file prevents a stale shared descriptor from
	// inheriting credentials. A missing descriptor is harmless: bbolt stays locked.
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return errors.New("cannot create local control descriptor name")
	}
	temp := path + "." + hex.EncodeToString(suffix[:]) + ".tmp"
	f, err := fileio.CreatePrivate(temp)
	if err != nil {
		return errors.New("cannot create private local control descriptor")
	}
	defer os.Remove(temp)
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return errors.New("cannot write local control descriptor")
	}
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return errors.New("local control descriptor is not a regular file")
		}
		if err := os.Remove(path); err != nil {
			return errors.New("cannot replace local control descriptor")
		}
	} else if !os.IsNotExist(err) {
		return errors.New("cannot inspect local control descriptor")
	}
	if err := os.Rename(temp, path); err != nil {
		return errors.New("cannot publish local control descriptor")
	}
	return nil
}

func readDescriptor(path string) (descriptor, error) {
	var d descriptor
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return d, ErrUnavailable
	}
	if err != nil {
		return d, errors.New("cannot read local control descriptor")
	}
	if !info.Mode().IsRegular() || info.Size() > maxDescriptorBytes {
		return d, errors.New("invalid local control descriptor")
	}
	if err := checkPrivateDescriptor(path, info); err != nil {
		return d, err
	}
	f, err := fileio.OpenSnapshot(path)
	if err != nil {
		if os.IsNotExist(err) {
			return d, ErrUnavailable
		}
		return d, errors.New("cannot read local control descriptor")
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxDescriptorBytes+1))
	if err != nil || len(data) > maxDescriptorBytes || decodeStrict(data, &d) != nil {
		return d, errors.New("invalid local control descriptor")
	}
	if d.Version != protocolVersion || len(d.Token) != 64 || d.PID < 1 {
		return d, errors.New("unsupported local control descriptor")
	}
	if _, err := hex.DecodeString(d.Token); err != nil {
		return d, errors.New("invalid local control descriptor")
	}
	if err := validEndpoint(d.Endpoint); err != nil {
		return d, err
	}
	return d, nil
}

func validEndpoint(endpoint string) error {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "http" || u.User != nil || u.Hostname() != "127.0.0.1" || u.Path != "/v1/command" || u.RawPath != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return errors.New("invalid local control endpoint")
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 || u.Host != "127.0.0.1:"+strconv.Itoa(port) {
		return errors.New("invalid local control endpoint")
	}
	return nil
}

func localHandler(token, host string, handler Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if r.Method != http.MethodPost || r.URL.Path != "/v1/command" || r.URL.RawQuery != "" || r.Host != host || r.Header.Get("Origin") != "" {
			http.Error(w, "invalid local control request", http.StatusBadRequest)
			return
		}
		remote, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil || !net.ParseIP(remote).IsLoopback() {
			http.Error(w, "local requests only", http.StatusForbidden)
			return
		}
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+token)) != 1 {
			http.Error(w, "local authentication failed", http.StatusUnauthorized)
			return
		}
		if r.Header.Get("Content-Type") != "application/json" {
			http.Error(w, "JSON required", http.StatusUnsupportedMediaType)
			return
		}
		data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBytes))
		var request Request
		if err != nil || decodeStrict(data, &request) != nil || ValidateRequest(request) != nil {
			writeResponse(w, nil, ErrInvalid)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
		defer cancel()
		result, err := handler(ctx, request)
		writeResponse(w, result, err)
	})
}

func writeResponse(w http.ResponseWriter, data any, err error) {
	status := http.StatusOK
	result := response{}
	if err != nil {
		switch {
		case errors.Is(err, ErrInvalid):
			status = http.StatusBadRequest
			result.Error = "invalid_request"
		case errors.Is(err, ErrNotFound), errors.Is(err, state.ErrDeliveryMissing):
			status = http.StatusNotFound
			result.Error = "not_found"
		case errors.Is(err, state.ErrInFlight):
			status = http.StatusConflict
			result.Error = "in_flight"
		case errors.Is(err, state.ErrSuperseded):
			status = http.StatusConflict
			result.Error = "superseded"
		case errors.Is(err, state.ErrAlreadyPending):
			status = http.StatusConflict
			result.Error = "already_pending"
		case errors.Is(err, state.ErrNotRetryable):
			status = http.StatusConflict
			result.Error = "not_failed"
		case errors.Is(err, state.ErrDeliveryIneligible):
			status = http.StatusConflict
			result.Error = "ineligible"
		case err.Error() == "cursor does not match this query", err.Error() == "invalid cursor":
			status = http.StatusBadRequest
			result.Error = "cursor_mismatch"
		case err.Error() == "event ID is ambiguous; specify provider":
			status = http.StatusBadRequest
			result.Error = "ambiguous_event"
		case errors.Is(err, ErrBusy), errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
			status = http.StatusConflict
			result.Error = "busy"
		default:
			status = http.StatusConflict
			result.Error = "operation_failed"
		}
	} else {
		result.Data, err = json.Marshal(data)
		if err != nil {
			status = http.StatusInternalServerError
			result = response{Error: "encoding_failed"}
		}
	}
	body, err := json.Marshal(result)
	if err != nil || len(body) > maxResponseBytes {
		status = http.StatusRequestEntityTooLarge
		body = []byte(`{"error":"response_too_large"}`)
	}
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func decodeStrict(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return ErrInvalid
	}
	return nil
}

// Call never uses proxy discovery or follows redirects, including when the
// operator has HTTP_PROXY set for ordinary external API traffic.
func Call(ctx context.Context, statePath string, request Request, result any) error {
	if err := ValidateRequest(request); err != nil {
		return err
	}
	d, err := readDescriptor(DescriptorPath(statePath))
	if err != nil {
		return err
	}
	body, err := json.Marshal(request)
	if err != nil || len(body) > maxRequestBytes {
		return ErrInvalid
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.Endpoint, bytes.NewReader(body))
	if err != nil {
		return errors.New("cannot create local control request")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+d.Token)
	transport := &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: time.Second}).DialContext, DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{Timeout: 5 * time.Second, Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Do(req)
	if err != nil {
		if connectionRefused(err) {
			return ErrUnavailable
		}
		if IsMutation(request.Operation) {
			return errors.New("local control request failed or timed out; delivery may already have been requeued")
		}
		return errors.New("local control request failed or timed out")
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, maxResponseBytes+1))
	if err != nil || len(data) > maxResponseBytes {
		return errors.New("local control response exceeds bounds or is unreadable")
	}
	var reply response
	if decodeStrict(data, &reply) != nil {
		return errors.New("invalid local control response")
	}
	if res.StatusCode != http.StatusOK || reply.Error != "" {
		switch reply.Error {
		case "not_found":
			return ErrNotFound
		case "invalid_request":
			return ErrInvalid
		case "busy":
			return ErrBusy
		case "in_flight":
			return state.ErrInFlight
		case "superseded":
			return state.ErrSuperseded
		case "already_pending":
			return state.ErrAlreadyPending
		case "not_failed":
			return state.ErrNotRetryable
		case "ineligible":
			return state.ErrDeliveryIneligible
		case "cursor_mismatch":
			return errors.New("cursor does not match this query; keep the same filters when requesting the next page")
		case "ambiguous_event":
			return errors.New("event ID is ambiguous; specify --provider")
		case "response_too_large":
			return errors.New("response is too large; request a smaller page")
		default:
			return errors.New("local management operation was rejected; inspect delivery eligibility or daemon status")
		}
	}
	if len(reply.Data) == 0 || strings.TrimSpace(string(reply.Data)) == "null" || json.Unmarshal(reply.Data, result) != nil {
		return errors.New("invalid local management result")
	}
	return nil
}
