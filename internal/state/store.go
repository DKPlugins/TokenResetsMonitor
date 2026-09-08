// Package state owns the transactional event history and durable outbox.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/DKPlugins/TokenResetsMonitor/internal/fileio"
	"github.com/DKPlugins/TokenResetsMonitor/internal/model"
	bolt "go.etcd.io/bbolt"
)

const SchemaVersion = 3

var ErrLocked = errors.New("state database is in use by a running monitor")

var buckets = []string{"meta", "providers", "events", "outbox", "cache", "cooldowns", "attempts", "revisions"}

type Store struct {
	db          *bolt.DB
	path        string
	statusMu    sync.Mutex
	runtimeMu   sync.RWMutex
	runtimeJSON []byte
	policyMu    sync.RWMutex
	policy      Policy
}

// Open takes a bounded exclusive lock. An existing database is inspected read-only
// before opening it for writes, so a newer schema is never modified.
func Open(path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("state path is required")
	}
	info, err := os.Stat(path)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	exists := err == nil && info.Size() > 0
	if exists {
		probe, err := bolt.Open(path, 0600, &bolt.Options{ReadOnly: true, Timeout: time.Second})
		if err != nil {
			return nil, openError(err)
		}
		version, err := schema(probe)
		if err == nil && version > SchemaVersion {
			err = fmt.Errorf("state schema %d is newer than supported schema %d", version, SchemaVersion)
		}
		closeErr := probe.Close()
		if err != nil {
			return nil, err
		}
		if closeErr != nil {
			return nil, closeErr
		}
	} else if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	db, err := bolt.Open(path, 0600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		return nil, openError(err)
	}
	s := &Store{db: db, path: path}
	version, err := schema(db)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	if version > SchemaVersion {
		_ = db.Close()
		return nil, fmt.Errorf("unsupported state schema %d", version)
	}
	if exists && version < SchemaVersion {
		backup := path + ".backup-" + time.Now().UTC().Format("20060102T150405.000000000Z")
		if err := db.View(func(tx *bolt.Tx) error { return tx.CopyFile(backup, 0600) }); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("backup before state migration: %w", err)
		}
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		for _, name := range buckets {
			if _, err := tx.CreateBucketIfNotExists([]byte(name)); err != nil {
				return err
			}
		}
		// Schema three adds immutable attempt history and source decisions.
		// Older schemas were backed up before this transaction.
		if err := migrateHistory(tx, version); err != nil {
			return err
		}
		if err := recoverAttempts(tx, time.Now()); err != nil {
			return err
		}
		return tx.Bucket([]byte("meta")).Put([]byte("state_schema_version"), []byte(strconv.Itoa(SchemaVersion)))
	}); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func openError(err error) error {
	if errors.Is(err, bolt.ErrTimeout) {
		return ErrLocked
	}
	return err
}

func schema(db *bolt.DB) (int, error) {
	version := 0
	err := db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("meta"))
		if b == nil {
			return nil
		}
		value := b.Get([]byte("state_schema_version"))
		if value == nil {
			return nil
		}
		v, err := strconv.Atoi(string(value))
		if err != nil || v < 0 {
			return errors.New("invalid state schema version")
		}
		version = v
		return nil
	})
	return version, err
}

func (s *Store) Close() error { return s.db.Close() }

func getJSON(b *bolt.Bucket, key string, target any) (bool, error) {
	if b == nil {
		return false, nil
	}
	data := b.Get([]byte(key))
	if data == nil {
		return false, nil
	}
	return true, json.Unmarshal(data, target)
}

func putJSON(b *bolt.Bucket, key string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return b.Put([]byte(key), data)
}

// forEachSnapshot permits replacing values safely without mutating a live
// bbolt cursor, whose traversal is undefined when the bucket is changed.
func forEachSnapshot(b *bolt.Bucket, fn func([]byte, []byte) error) error {
	type entry struct{ key, value []byte }
	var entries []entry
	if err := b.ForEach(func(k, v []byte) error {
		entries = append(entries, entry{append([]byte(nil), k...), append([]byte(nil), v...)})
		return nil
	}); err != nil {
		return err
	}
	for _, item := range entries {
		if err := fn(item.key, item.value); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) GetCache(key string) (model.CachedResponse, bool, error) {
	var response model.CachedResponse
	var found bool
	err := s.db.View(func(tx *bolt.Tx) error {
		var err error
		found, err = getJSON(tx.Bucket([]byte("cache")), key, &response)
		return err
	})
	return response, found, err
}

func (s *Store) PutCache(key string, response model.CachedResponse) error {
	return s.db.Update(func(tx *bolt.Tx) error { return putJSON(tx.Bucket([]byte("cache")), key, response) })
}

// PublishStatusWithRuntime updates the immutable runtime metadata used by all
// subsequent status snapshots. Passing nil retains the previous metadata.
func (s *Store) PublishStatusWithRuntime(running bool, now time.Time, runtime *model.RuntimeStatus) error {
	if runtime != nil {
		data, err := json.Marshal(runtime)
		if err != nil {
			return err
		}
		s.runtimeMu.Lock()
		s.runtimeJSON = data
		s.runtimeMu.Unlock()
	}
	return s.PublishStatus(running, now)
}

func (s *Store) runtimeStatus() (*model.RuntimeStatus, error) {
	s.runtimeMu.RLock()
	data := s.runtimeJSON
	s.runtimeMu.RUnlock()
	if len(data) == 0 {
		return nil, nil
	}
	var runtime model.RuntimeStatus
	if err := json.Unmarshal(data, &runtime); err != nil {
		return nil, err
	}
	return &runtime, nil
}

// PublishStatus publishes only operational metadata, allowing status and log
// export to work without taking the daemon's database lock.
func (s *Store) PublishStatus(running bool, now time.Time) error {
	s.statusMu.Lock()
	defer s.statusMu.Unlock()
	status, err := s.Status(running, now)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(status, "", "  ")
	if err != nil {
		return err
	}
	target := s.path + ".status.json"
	f, err := os.CreateTemp(filepath.Dir(target), ".status-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(0600); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return replaceStatusFile(tmp, target)
}

func ReadStatus(path string) (model.Status, error) {
	const maxStatusBytes = 1 << 20
	var status model.Status
	reader, err := fileio.OpenSnapshot(path + ".status.json")
	if err != nil {
		return status, err
	}
	defer reader.Close()
	data, err := io.ReadAll(io.LimitReader(reader, maxStatusBytes+1))
	if err != nil {
		return status, err
	}
	if len(data) > maxStatusBytes {
		return status, errors.New("status snapshot exceeds size limit")
	}
	err = json.Unmarshal(data, &status)
	return status, err
}
