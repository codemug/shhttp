// Package eventlog stores a session's events in an append-only file of JSON
// lines and serves them to any number of readers.
//
// Recent events are also kept in memory. Readers pull events by sequence
// number: a reader that keeps up is served from memory, one that falls behind
// or starts from the beginning is served from disk. Writers never wait for
// readers.
package eventlog

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/codemug/shhttp/pkg/api"
)

const (
	// indexEvery is the spacing of the sparse seq → file offset index.
	indexEvery = 128
	// The in-memory tail keeps at most this many events or bytes of data.
	tailMaxEvents = 4096
	tailMaxBytes  = 4 << 20
)

// ErrClosed is returned by Append after Close.
var ErrClosed = errors.New("eventlog: closed")

type indexEntry struct {
	seq    uint64
	offset int64
}

// Log is one session's event log. It is safe for concurrent use.
type Log struct {
	path string

	mu        sync.Mutex
	w         *os.File // nil once closed
	closed    bool
	last      uint64
	size      int64
	index     []indexEntry
	tail      []api.Event
	tailBytes int
	changed   chan struct{}
}

// Create creates a new, empty log file at path.
func Create(path string) (*Log, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	return &Log{path: path, w: f, changed: make(chan struct{})}, nil
}

// Open opens an existing log read-only, for example the log of a session
// that ended before the server restarted. A truncated last line, left by a
// crash, is ignored.
func Open(path string) (*Log, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	l := &Log{path: path, closed: true, changed: make(chan struct{})}
	r := bufio.NewReader(f)
	var off int64
	for {
		line, err := r.ReadBytes('\n')
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		var e api.Event
		if err := json.Unmarshal(line, &e); err != nil {
			return nil, fmt.Errorf("eventlog: %s at offset %d: %w", path, off, err)
		}
		if e.Seq != l.last+1 {
			return nil, fmt.Errorf("eventlog: %s: expected seq %d, found %d", path, l.last+1, e.Seq)
		}
		l.record(e, off)
		off += int64(len(line))
	}
	l.size = off
	return l, nil
}

// OpenAppend opens an existing log to append more events, for example the
// log of a job resumed after a restart.
func OpenAppend(path string) (*Log, error) {
	l, err := Open(path)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	if err := f.Truncate(l.size); err != nil { // drop a torn last line
		f.Close()
		return nil, err
	}
	l.w, l.closed = f, false
	return l, nil
}

// Path returns the log's file path.
func (l *Log) Path() string { return l.path }

// Append assigns the next sequence number to e, stamps its time if unset,
// writes it and wakes waiting readers.
func (l *Log) Append(e api.Event) (api.Event, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return e, ErrClosed
	}
	e.Seq = l.last + 1
	if e.Time.IsZero() {
		e.Time = time.Now().UTC()
	}
	line, err := api.Marshal(e)
	if err != nil {
		return e, err
	}
	line = append(line, '\n')
	if _, err := l.w.Write(line); err != nil {
		return e, err
	}
	l.record(e, l.size)
	l.size += int64(len(line))
	l.notify()
	return e, nil
}

// record adds e, stored at offset, to the index and tail. Callers hold mu.
func (l *Log) record(e api.Event, offset int64) {
	l.last = e.Seq
	if (e.Seq-1)%indexEvery == 0 {
		l.index = append(l.index, indexEntry{e.Seq, offset})
	}
	l.tail = append(l.tail, e)
	l.tailBytes += len(e.Data)
	drop := 0
	for len(l.tail)-drop > tailMaxEvents || (l.tailBytes > tailMaxBytes && len(l.tail)-drop > 1) {
		l.tailBytes -= len(l.tail[drop].Data)
		drop++
	}
	if drop > 0 {
		clear(l.tail[:drop])
		l.tail = l.tail[drop:]
	}
}

func (l *Log) notify() {
	close(l.changed)
	l.changed = make(chan struct{})
}

// Close marks the log complete. Readers that are following it receive the
// remaining events and then io.EOF.
func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	l.closed = true
	l.notify()
	err := l.w.Close()
	l.w = nil
	return err
}

// Last returns the sequence number of the newest event, or 0 if the log is
// empty.
func (l *Log) Last() uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.last
}

// Read returns up to max events starting at sequence number from (0 is
// treated as 1). When no such event exists yet it returns io.EOF if the log
// is closed or follow is false; otherwise it waits for one to be appended or
// for ctx to end.
func (l *Log) Read(ctx context.Context, from uint64, max int, follow bool) ([]api.Event, error) {
	if from == 0 {
		from = 1
	}
	if max <= 0 {
		max = 256
	}
	for {
		l.mu.Lock()
		if from <= l.last {
			if len(l.tail) > 0 && from >= l.tail[0].Seq {
				i := int(from - l.tail[0].Seq)
				n := min(max, len(l.tail)-i)
				out := make([]api.Event, n)
				copy(out, l.tail[i:i+n])
				l.mu.Unlock()
				return out, nil
			}
			start := l.lookup(from)
			size := l.size
			l.mu.Unlock()
			return l.readDisk(start, size, from, max)
		}
		if l.closed || !follow {
			l.mu.Unlock()
			return nil, io.EOF
		}
		ch := l.changed
		l.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// lookup returns the index entry at or before seq. Callers hold mu.
func (l *Log) lookup(seq uint64) indexEntry {
	i := sort.Search(len(l.index), func(i int) bool { return l.index[i].seq > seq })
	return l.index[i-1]
}

func (l *Log) readDisk(start indexEntry, size int64, from uint64, max int) ([]api.Event, error) {
	f, err := os.Open(l.path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := bufio.NewReader(io.NewSectionReader(f, start.offset, size-start.offset))
	var out []api.Event
	for len(out) < max {
		line, err := r.ReadBytes('\n')
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		var e api.Event
		if err := json.Unmarshal(line, &e); err != nil {
			return nil, err
		}
		if e.Seq >= from {
			out = append(out, e)
		}
	}
	return out, nil
}
