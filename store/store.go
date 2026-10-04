// Package store provides a durable write-ahead log of committed registry
// events and atomic snapshot files used for restart recovery.
//
// The WAL is a sequence of newline-delimited JSON records ("wal.log").
// Every commit is appended with a strictly increasing Seq and fsynced before
// the commit is acknowledged, so replay in Seq order reconstructs exactly the
// committed prefix: a torn final record is ignored.
package store

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// EntryType tags a WAL entry.
type EntryType string

const (
	// EntryRegister records a new metric version (full Definition payload).
	EntryRegister EntryType = "register"
	// EntryRetire records a metric being taken offline.
	EntryRetire EntryType = "retire"
	// EntryBaseUpdate records committed base data upserts.
	EntryBaseUpdate EntryType = "base_update"
	// EntryResults records a committed recomputation result snapshot.
	EntryResults EntryType = "results"
	// EntrySnapshot is a compacted snapshot checkpoint (payload opaque).
	EntrySnapshot EntryType = "snapshot"
)

// RegisterPayload is the payload of an EntryRegister record.
type RegisterPayload struct {
	DefJSON json.RawMessage `json:"def"`
}

// RetirePayload is the payload of an EntryRetire record.
type RetirePayload struct {
	ID string `json:"id"`
}

// BaseUpdatePayload is the payload of an EntryBaseUpdate record.
type BaseUpdatePayload struct {
	ID     string             `json:"id"`
	Points map[string]float64 `json:"points"`
}

// ResultsPayload is the payload of an EntryResults record.
type ResultsPayload struct {
	Results map[string]map[string]float64 `json:"results"`
	Dirty   []string                      `json:"dirty"`
}

// SnapshotPayload is an opaque compaction checkpoint.
type SnapshotPayload struct {
	Data json.RawMessage `json:"data"`
}

// Entry is one durable WAL record.
type Entry struct {
	Seq     int64           `json:"seq"`
	Type    EntryType       `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// WAL is an append-only, fsynced log of committed entries.
type WAL struct {
	mu   sync.Mutex
	path string
	f    *os.File
	w    *bufio.Writer
	seq  int64
}

// OpenWAL opens (creating if needed) the WAL file at dir/wal.log and scans it
// to learn the last sequence number.
func OpenWAL(dir string) (*WAL, error) {
	if dir == "" {
		return nil, errors.New("store: directory required")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "wal.log")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	w := &WAL{path: path, f: f, w: bufio.NewWriter(f)}
	if err := w.bootstrap(); err != nil {
		_ = f.Close()
		return nil, err
	}
	return w, nil
}

func (w *WAL) bootstrap() error {
	var lastSeq int64
	err := w.Replay(func(e *Entry) error {
		if e.Seq > lastSeq {
			lastSeq = e.Seq
		}
		return nil
	})
	if err != nil {
		return err
	}
	w.seq = lastSeq
	return nil
}

// Append durably appends one entry; Seq is assigned automatically.  payload
// may be a json.RawMessage (used verbatim) or any JSON-encodable value.
func (w *WAL) Append(typ EntryType, payload interface{}) (*Entry, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	var raw json.RawMessage
	switch p := payload.(type) {
	case nil:
	case json.RawMessage:
		raw = p
	case []byte:
		raw = json.RawMessage(p)
	default:
		b, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		raw = b
	}
	w.seq++
	e := &Entry{Seq: w.seq, Type: typ, Payload: raw}
	line, err := json.Marshal(e)
	if err != nil {
		w.seq--
		return nil, err
	}
	if _, err := w.w.Write(line); err != nil {
		w.seq--
		return nil, err
	}
	if err := w.w.WriteByte('\n'); err != nil {
		w.seq--
		return nil, err
	}
	if err := w.w.Flush(); err != nil {
		w.seq--
		return nil, err
	}
	if err := w.f.Sync(); err != nil {
		return nil, err
	}
	return e, nil
}

// Replay replays all intact entries in order.  A truncated/torn final line is
// ignored: only fully committed records are observed.
func (w *WAL) Replay(fn func(*Entry) error) error {
	f, err := os.Open(w.path)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 64*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e Entry
		if err := json.Unmarshal(line, &e); err != nil {
			// Torn record at tail: stop replay at the last good record.
			break
		}
		if err := fn(&e); err != nil {
			return err
		}
	}
	return sc.Err()
}

// LastSeq returns the last appended sequence number.
func (w *WAL) LastSeq() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.seq
}

// Close flushes and closes the underlying file.
func (w *WAL) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return nil
	}
	if err := w.w.Flush(); err != nil {
		return err
	}
	if err := w.f.Sync(); err != nil {
		return err
	}
	err := w.f.Close()
	w.f = nil
	return err
}

var _ io.Closer = (*WAL)(nil)

// WriteSnapshot atomically writes data to dir/name (write temp + rename +
// fsync of the directory).
func WriteSnapshot(dir, name string, data []byte) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp := filepath.Join(dir, name+".tmp")
	final := filepath.Join(dir, name)
	f, err := os.OpenFile(tmp, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
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
	if err := os.Rename(tmp, final); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = d.Sync()
	_ = d.Close()
	return err
}

// ReadSnapshot reads a snapshot file; a missing file returns (nil, nil).
func ReadSnapshot(dir, name string) ([]byte, error) {
	data, err := os.ReadFile(filepath.Join(dir, name))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return data, nil
}

// DecodePayload unmarshals an entry payload into the given value.
func DecodePayload(e *Entry, v interface{}) error {
	if len(e.Payload) == 0 {
		return fmt.Errorf("store: entry %d has no payload", e.Seq)
	}
	return json.Unmarshal(e.Payload, v)
}
