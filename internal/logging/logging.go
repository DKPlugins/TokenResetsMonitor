// Package logging provides redacted structured output and bounded rotating files.
package logging

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/DKPlugins/TokenResetsMonitor/internal/config"
)

var urlPattern = regexp.MustCompile(`https?://[^\s<>"']+`)
var tokenPattern = regexp.MustCompile(`\b[0-9]{5,}:[A-Za-z0-9_-]{15,}\b`)

const maxLogRecordBytes = 1 << 20

type Redactor struct{ secrets []string }

func NewRedactor(cfg config.Config) *Redactor {
	values := []string{cfg.Webhook.URL, cfg.Telegram.BotToken, cfg.Telegram.ChatID, cfg.Webhook.BodyTemplate, cfg.Slack.WebhookURL}
	for _, value := range cfg.Webhook.Headers {
		values = append(values, value)
		if strings.HasPrefix(value, "Bearer ") {
			values = append(values, strings.TrimPrefix(value, "Bearer "))
		}
	}
	if u, e := url.Parse(cfg.Webhook.URL); e == nil {
		if u.User != nil {
			values = append(values, u.User.Username())
			p, _ := u.User.Password()
			values = append(values, p)
		}
		for _, vs := range u.Query() {
			values = append(values, vs...)
		}
	}
	sort.Slice(values, func(i, j int) bool { return len(values[i]) > len(values[j]) })
	r := &Redactor{}
	for _, v := range values {
		if v != "" {
			r.secrets = append(r.secrets, v)
		}
	}
	return r
}

func (r *Redactor) Text(s string) string {
	for _, v := range r.secrets {
		s = strings.ReplaceAll(s, v, "[redacted]")
	}
	s = urlPattern.ReplaceAllString(s, "[url redacted]")
	return tokenPattern.ReplaceAllString(s, "[token redacted]")
}

func (r *Redactor) Attr(groups []string, a slog.Attr) slog.Attr {
	for _, group := range groups {
		if sensitiveKey(group) {
			return slog.String(a.Key, "[redacted]")
		}
	}
	key := strings.ToLower(a.Key)
	if sensitiveKey(key) {
		return slog.String(a.Key, "[redacted]")
	}
	if a.Value.Kind() == slog.KindString {
		return slog.String(a.Key, r.Text(a.Value.String()))
	}
	if a.Value.Kind() == slog.KindTime {
		return slog.Time(a.Key, a.Value.Time().UTC())
	}
	if a.Value.Kind() == slog.KindAny {
		return slog.String(a.Key, r.Text(fmt.Sprint(a.Value.Any())))
	}
	return a
}

func sensitiveKey(key string) bool {
	switch strings.ToLower(key) {
	case "headers", "authorization", "cookie", "token", "bot_token", "access_token", "api_key", "password", "body", "request_body", "response_body", "request", "response", "config", "event", "notification":
		return true
	}
	return false
}

// slog discards Handler.Handle errors. Keep a sticky failure for shutdown and
// report the first failure directly through a separate, non-recursive sink.
type outputFailures struct {
	mu          sync.Mutex
	failed      bool
	diagnostics io.Writer
}

func (f *outputFailures) report() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.failed {
		f.failed = true
		fmt.Fprintln(f.diagnostics, "TokenResetsMonitor: log output failed; check permissions, record size, and available disk space.")
	}
}
func (f *outputFailures) err() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failed {
		return errors.New("log output failed during this run")
	}
	return nil
}

type multiHandler struct {
	handlers []slog.Handler
	failures *outputFailures
}

func (h multiHandler) Enabled(ctx context.Context, l slog.Level) bool {
	for _, v := range h.handlers {
		if v.Enabled(ctx, l) {
			return true
		}
	}
	return false
}
func (h multiHandler) Handle(ctx context.Context, r slog.Record) error {
	var first error
	for _, v := range h.handlers {
		if v.Enabled(ctx, r.Level) {
			if e := v.Handle(ctx, r.Clone()); e != nil && first == nil {
				first = e
				if h.failures != nil {
					h.failures.report()
				}
			}
		}
	}
	return first
}
func (h multiHandler) WithAttrs(a []slog.Attr) slog.Handler {
	n := multiHandler{failures: h.failures}
	for _, v := range h.handlers {
		n.handlers = append(n.handlers, v.WithAttrs(a))
	}
	return n
}
func (h multiHandler) WithGroup(g string) slog.Handler {
	n := multiHandler{failures: h.failures}
	for _, v := range h.handlers {
		n.handlers = append(n.handlers, v.WithGroup(g))
	}
	return n
}

type RuntimeControl struct {
	mu       sync.Mutex
	level    slog.LevelVar
	redactor atomic.Pointer[Redactor]
	previous *Redactor
	settings config.Logging
}

// Apply changes the level and secret redaction without opening another writer.
// Changes to file layout/format must be rejected by the runtime reload policy.
func (c *RuntimeControl) Apply(cfg config.Config) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	settings := cfg.Logging
	settings.Level = c.settings.Level
	if settings != c.settings {
		return errors.New("logging output settings require a restart")
	}
	var level slog.Level
	if err := level.UnmarshalText([]byte(cfg.Logging.Level)); err != nil {
		return errors.New("invalid log level")
	}
	next := NewRedactor(cfg)
	combined := &Redactor{secrets: append([]string{}, next.secrets...)}
	if c.previous != nil {
		combined.secrets = append(combined.secrets, c.previous.secrets...)
	}
	sort.Slice(combined.secrets, func(i, j int) bool { return len(combined.secrets[i]) > len(combined.secrets[j]) })
	c.redactor.Store(combined)
	c.previous = next
	c.level.Set(level)
	return nil
}

func New(cfg config.Config, stdout io.Writer, diagnostics ...io.Writer) (*slog.Logger, func() error, error) {
	logger, _, closeFn, err := NewRuntime(cfg, stdout, diagnostics...)
	return logger, closeFn, err
}

func NewRuntime(cfg config.Config, stdout io.Writer, diagnostics ...io.Writer) (*slog.Logger, *RuntimeControl, func() error, error) {
	control := &RuntimeControl{settings: cfg.Logging}
	if err := control.Apply(cfg); err != nil {
		return nil, nil, nil, err
	}
	opts := &slog.HandlerOptions{Level: &control.level, ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
		return control.redactor.Load().Attr(groups, a)
	}}
	var console slog.Handler = slog.NewTextHandler(stdout, opts)
	if cfg.Logging.Format == "json" {
		console = slog.NewJSONHandler(stdout, opts)
	}
	var diagnosticOutput io.Writer = os.Stderr
	if len(diagnostics) > 0 && diagnostics[0] != nil {
		diagnosticOutput = diagnostics[0]
	}
	failures := &outputFailures{diagnostics: diagnosticOutput}
	h := multiHandler{handlers: []slog.Handler{console}, failures: failures}
	closeWriter := func() error { return nil }
	if cfg.Logging.FileEnabled {
		w := &lazyRotator{cfg: cfg.Logging}
		h.handlers = append(h.handlers, slog.NewJSONHandler(w, opts))
		closeWriter = w.Close
	}
	closeFn := func() error {
		if err := closeWriter(); err != nil {
			failures.report()
		}
		return failures.err()
	}
	return slog.New(h), control, closeFn, nil
}

// Opening is delayed until the monitor has acquired its exclusive state lock.
type lazyRotator struct {
	mu     sync.Mutex
	cfg    config.Logging
	writer *Rotator
}

func (w *lazyRotator) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.writer == nil {
		r, e := NewRotator(w.cfg)
		if e != nil {
			return 0, fmt.Errorf("cannot initialize file logging")
		}
		w.writer = r
	}
	return w.writer.Write(p)
}
func (w *lazyRotator) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.writer == nil {
		return nil
	}
	return w.writer.Close()
}

// Rotator is single-process; auxiliary commands must never open this writer.
type Rotator struct {
	mu        sync.Mutex
	cfg       config.Logging
	file      *os.File
	size      int64
	lastPrune time.Time
	closed    bool
}

func NewRotator(cfg config.Logging) (*Rotator, error) {
	if cfg.MaxSizeMB < 1 || cfg.MaxBackups < 1 || cfg.MaxAgeDays < 1 {
		return nil, fmt.Errorf("invalid rotation limits")
	}
	if e := os.MkdirAll(cfg.Directory, 0700); e != nil {
		return nil, e
	}
	r := &Rotator{cfg: cfg}
	if e := r.open(); e != nil {
		return nil, e
	}
	if err := r.prune(); err != nil {
		r.Close()
		return nil, err
	}
	return r, nil
}
func (r *Rotator) path() string { return filepath.Join(r.cfg.Directory, "tokenresetsmonitor.jsonl") }
func (r *Rotator) open() error {
	f, e := os.OpenFile(r.path(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	st, e := f.Stat()
	if e != nil {
		f.Close()
		return e
	}
	r.file = f
	r.size = st.Size()
	return nil
}
func (r *Rotator) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return 0, fmt.Errorf("log file is closed")
	}
	if r.file == nil {
		if err := r.open(); err != nil {
			return 0, errors.New("cannot reopen log file")
		}
	}
	if len(p) > maxLogRecordBytes {
		return 0, errors.New("log record exceeds 1 MiB limit")
	}
	if r.size > 0 && r.size+int64(len(p)) > int64(r.cfg.MaxSizeMB)*1024*1024 {
		if e := r.rotate(); e != nil {
			return 0, e
		}
	}
	n, e := r.file.Write(p)
	r.size += int64(n)
	if time.Since(r.lastPrune) > time.Hour {
		if err := r.prune(); e == nil {
			e = err
		}
	}
	return n, e
}
func (r *Rotator) rotate() error {
	if e := r.file.Sync(); e != nil {
		return e
	}
	e := r.file.Close()
	r.file = nil
	if e != nil {
		return e
	}
	archive := filepath.Join(r.cfg.Directory, "tokenresetsmonitor-"+time.Now().UTC().Format("20060102T150405.000000000Z")+".jsonl")
	if e := os.Rename(r.path(), archive); e != nil {
		_ = r.open()
		return e
	}
	if e := r.open(); e != nil {
		return e
	}
	if e := compressFile(archive); e != nil {
		return e
	}
	return r.prune()
}
func compressFile(path string) error {
	in, e := os.Open(path)
	if e != nil {
		return e
	}
	defer in.Close()
	out, e := os.OpenFile(path+".gz", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	gz := gzip.NewWriter(out)
	_, e = io.Copy(gz, in)
	ge := gz.Close()
	ce := out.Close()
	in.Close()
	if e != nil || ge != nil || ce != nil {
		os.Remove(path + ".gz")
		return fmt.Errorf("cannot compress rotated log")
	}
	return os.Remove(path)
}
func (r *Rotator) prune() error {
	r.lastPrune = time.Now()
	entries, err := os.ReadDir(r.cfg.Directory)
	if err != nil {
		return errors.New("cannot inspect rotated log retention")
	}
	var archives []os.DirEntry
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), "tokenresetsmonitor-") && (strings.HasSuffix(e.Name(), ".jsonl.gz") || strings.HasSuffix(e.Name(), ".jsonl")) {
			archives = append(archives, e)
		}
	}
	sort.Slice(archives, func(i, j int) bool { return archives[i].Name() > archives[j].Name() })
	cutoff := time.Now().Add(-time.Duration(r.cfg.MaxAgeDays) * 24 * time.Hour)
	var failure error
	for i, e := range archives {
		info, err := e.Info()
		if err == nil && (i >= r.cfg.MaxBackups || info.ModTime().Before(cutoff)) {
			if err := os.Remove(filepath.Join(r.cfg.Directory, e.Name())); err != nil {
				failure = errors.New("cannot remove expired rotated log")
			}
		}
	}
	return failure
}
func (r *Rotator) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	if r.file == nil {
		return nil
	}
	e := r.file.Sync()
	ce := r.file.Close()
	r.file = nil
	if e != nil {
		return e
	}
	return ce
}
