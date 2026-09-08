package logging

import (
	"archive/zip"
	"bufio"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/DKPlugins/TokenResetsMonitor/internal/buildinfo"
	"github.com/DKPlugins/TokenResetsMonitor/internal/config"
	"github.com/DKPlugins/TokenResetsMonitor/internal/fileio"
	"github.com/DKPlugins/TokenResetsMonitor/internal/model"
)

type ExportOptions struct {
	Since  time.Duration
	Output string
	Input  string
	Stdin  io.Reader
}

type exportManifest struct {
	CreatedAt      time.Time  `json:"created_at"`
	RequestedSince time.Time  `json:"requested_since"`
	Version        string     `json:"version"`
	Commit         string     `json:"commit"`
	BuildDate      string     `json:"build_date"`
	Records        int        `json:"records"`
	Skipped        int        `json:"skipped"`
	Earliest       *time.Time `json:"earliest_available,omitempty"`
	Latest         *time.Time `json:"latest_available,omitempty"`
	Notes          []string   `json:"notes"`
}

// Export never includes arbitrary files or the raw config/database/environment.
func Export(cfg config.Config, opts ExportOptions) error {
	if opts.Since <= 0 || opts.Output == "" {
		return errors.New("a positive --since and --output are required")
	}
	if e := os.MkdirAll(filepath.Dir(opts.Output), 0700); e != nil {
		return errors.New("cannot create diagnostics directory")
	}
	out, e := createPrivateArchive(opts.Output)
	if e != nil {
		return errors.New("cannot create diagnostics archive; destination may already exist")
	}
	success := false
	defer func() {
		out.Close()
		if !success {
			os.Remove(opts.Output)
		}
	}()
	z := zip.NewWriter(out)
	defer z.Close()
	logEntry, e := z.Create("logs.jsonl")
	if e != nil {
		return errors.New("cannot create diagnostics entry")
	}
	now := time.Now().UTC()
	m := exportManifest{CreatedAt: now, RequestedSince: now.Add(-opts.Since), Version: buildinfo.Version, Commit: buildinfo.Commit, BuildDate: buildinfo.Date, Notes: []string{"Only available structured logs are included. Gaps and records lost before collection cannot be reconstructed.", "Configuration, environment, state database and network bodies are excluded."}}
	redactor := NewRedactor(cfg)
	consume := func(r io.Reader) error { return scanRecords(r, logEntry, redactor, &m) }
	if opts.Input != "" {
		if opts.Input == "-" {
			if opts.Stdin == nil {
				return errors.New("stdin is unavailable")
			}
			if err := consume(opts.Stdin); err != nil {
				return err
			}
		} else {
			f, err := fileio.OpenSnapshot(opts.Input)
			if err != nil {
				return errors.New("cannot read --input")
			}
			err = consume(fileSnapshot(f))
			f.Close()
			if err != nil {
				return err
			}
		}
	} else {
		entries, err := os.ReadDir(cfg.Logging.Directory)
		if err != nil {
			m.Notes = append(m.Notes, "File logs are unavailable; use --input with Docker or journald JSON output.")
		} else {
			sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
			for _, entry := range entries {
				name := entry.Name()
				if !entry.Type().IsRegular() {
					continue
				}
				if name != "tokenresetsmonitor.jsonl" && !(strings.HasPrefix(name, "tokenresetsmonitor-") && (strings.HasSuffix(name, ".jsonl") || strings.HasSuffix(name, ".jsonl.gz"))) {
					continue
				}
				f, err := fileio.OpenSnapshot(filepath.Join(cfg.Logging.Directory, name))
				if err != nil {
					m.Notes = append(m.Notes, "A rotated log was unavailable during collection.")
					continue
				}
				if strings.HasSuffix(name, ".gz") {
					gz, err := gzip.NewReader(fileSnapshot(f))
					if err == nil {
						err = consume(gz)
						gz.Close()
						if err != nil {
							f.Close()
							return err
						}
					} else {
						m.Skipped++
					}
				} else {
					if err = consume(fileSnapshot(f)); err != nil {
						f.Close()
						return err
					}
				}
				f.Close()
			}
		}
	}
	if m.Earliest == nil || m.Earliest.After(m.RequestedSince) {
		m.Notes = append(m.Notes, "Available records do not cover the entire requested period.")
	}
	if data, err := readStatusSnapshot(cfg.StatePath + ".status.json"); err == nil {
		var status model.Status
		if json.Unmarshal(data, &status) == nil {
			for key, p := range status.Providers {
				p.LastError = ""
				status.Providers[key] = p
			}
			entry, err := z.Create("status.json")
			if err != nil {
				return errors.New("cannot create diagnostics status")
			}
			if json.NewEncoder(entry).Encode(status) != nil {
				return errors.New("cannot write diagnostics status")
			}
		}
	}
	entry, e := z.Create("manifest.json")
	if e != nil {
		return errors.New("cannot create diagnostics manifest")
	}
	if e = json.NewEncoder(entry).Encode(m); e != nil {
		return errors.New("cannot write diagnostics manifest")
	}
	if e = z.Close(); e != nil {
		return errors.New("cannot finalize diagnostics archive")
	}
	if e = out.Sync(); e != nil {
		return errors.New("cannot sync diagnostics archive")
	}
	if e = out.Close(); e != nil {
		return errors.New("cannot close diagnostics archive")
	}
	success = true
	return nil
}

// A running daemon may keep appending. Export the bytes available when opened
// instead of following a growing file indefinitely.
func fileSnapshot(file *os.File) io.Reader {
	if info, err := file.Stat(); err == nil && info.Mode().IsRegular() {
		return io.NewSectionReader(file, 0, info.Size())
	}
	return file
}

func readStatusSnapshot(path string) ([]byte, error) {
	f, err := fileio.OpenSnapshot(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(fileSnapshot(f), maxLogRecordBytes+1))
	if err != nil || len(data) > maxLogRecordBytes {
		return nil, errors.New("status snapshot unavailable")
	}
	return data, nil
}

var exportFields = map[string]bool{"time": true, "level": true, "msg": true, "version": true, "commit": true, "date": true, "run_id": true, "cycle_id": true, "event_id": true, "notification_id": true, "delivery_id": true, "provider": true, "channel": true, "attempt": true, "duration": true, "duration_ms": true, "status": true, "status_code": true, "count": true, "events": true, "pending": true, "failed": true, "ready": true, "error": true, "reason": true, "retryable": true, "retry_after": true, "next_attempt": true, "once": true}

func scanRecords(reader io.Reader, writer io.Writer, r *Redactor, m *exportManifest) error {
	buffer := bufio.NewReaderSize(reader, 64*1024)
	oversizeReported := false
	for {
		line, oversized, readErr := readLogLine(buffer)
		if readErr != nil && readErr != io.EOF {
			m.Skipped++
			m.Notes = append(m.Notes, "A source ended with an unreadable record.")
			break
		}
		if len(line) == 0 && !oversized && readErr == io.EOF {
			break
		}
		if oversized {
			m.Skipped++
			if !oversizeReported {
				m.Notes = append(m.Notes, "Oversized records were skipped; later valid records were retained.")
				oversizeReported = true
			}
			if readErr == io.EOF {
				break
			}
			continue
		}
		var record map[string]any
		if json.Unmarshal(line, &record) != nil {
			m.Skipped++
			continue
		}
		raw, _ := record["time"].(string)
		stamp, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			m.Skipped++
			continue
		}
		if m.Earliest == nil || stamp.Before(*m.Earliest) {
			s := stamp
			m.Earliest = &s
		}
		if m.Latest == nil || stamp.After(*m.Latest) {
			s := stamp
			m.Latest = &s
		}
		if stamp.Before(m.RequestedSince) {
			continue
		}
		clean := map[string]any{}
		for key, value := range record {
			if !exportFields[key] {
				continue
			}
			switch v := value.(type) {
			case string:
				if key == "time" {
					clean[key] = stamp.UTC().Format(time.RFC3339Nano)
					continue
				}
				clean[key] = r.Text(v)
			case float64, bool, nil:
				clean[key] = v
			}
		}
		if json.NewEncoder(writer).Encode(clean) != nil {
			return errors.New("cannot write diagnostics log records")
		}
		m.Records++
	}
	return nil
}

func readLogLine(reader *bufio.Reader) ([]byte, bool, error) {
	var line []byte
	oversized := false
	for {
		part, err := reader.ReadSlice('\n')
		if !oversized {
			if len(line)+len(part) > maxLogRecordBytes {
				oversized = true
				line = nil
			} else {
				line = append(line, part...)
			}
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		return line, oversized, err
	}
}
