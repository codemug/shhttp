package eventlog

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/codemug/shhttp/pkg/api"
)

func newLog(t *testing.T) *Log {
	t.Helper()
	l, err := Create(filepath.Join(t.TempDir(), "events.log"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

func appendN(t *testing.T, l *Log, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, err := l.Append(api.Event{Type: api.EventStdout, Data: []byte(fmt.Sprintf("line %d\n", i))}); err != nil {
			t.Fatal(err)
		}
	}
}

func readAll(t *testing.T, l *Log, from uint64) []api.Event {
	t.Helper()
	var out []api.Event
	for {
		evs, err := l.Read(context.Background(), from, 100, false)
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, evs...)
		from = evs[len(evs)-1].Seq + 1
	}
}

func checkSeqs(t *testing.T, evs []api.Event, first, last uint64) {
	t.Helper()
	if len(evs) != int(last-first+1) {
		t.Fatalf("got %d events, want %d", len(evs), last-first+1)
	}
	for i, e := range evs {
		if e.Seq != first+uint64(i) {
			t.Fatalf("event %d has seq %d, want %d", i, e.Seq, first+uint64(i))
		}
		if want := fmt.Sprintf("line %d\n", e.Seq-1); string(e.Data) != want {
			t.Fatalf("seq %d has data %q, want %q", e.Seq, e.Data, want)
		}
	}
}

func TestReadFromMemoryAndDisk(t *testing.T) {
	l := newLog(t)
	n := tailMaxEvents + 3*indexEvery + 17
	appendN(t, l, n)
	// From the start: older events have left the in-memory tail.
	checkSeqs(t, readAll(t, l, 1), 1, uint64(n))
	// From the middle of an index interval on disk.
	checkSeqs(t, readAll(t, l, 200), 200, uint64(n))
	// From memory.
	checkSeqs(t, readAll(t, l, uint64(n-5)), uint64(n-5), uint64(n))
	if evs, err := l.Read(context.Background(), uint64(n+1), 10, false); err != io.EOF || evs != nil {
		t.Fatalf("read past end: %v, %v", evs, err)
	}
}

func TestBinaryDataRoundTrip(t *testing.T) {
	l := newLog(t)
	data := []byte{0xff, 0x00, 'a', 0xc3}
	if _, err := l.Append(api.Event{Type: api.EventStdout, Data: data}); err != nil {
		t.Fatal(err)
	}
	l.Close()
	r, err := Open(l.Path())
	if err != nil {
		t.Fatal(err)
	}
	evs := readAll(t, r, 1)
	if string(evs[0].Data) != string(data) {
		t.Fatalf("got %v, want %v", evs[0].Data, data)
	}
}

func TestOpenReplaysClosedLog(t *testing.T) {
	l := newLog(t)
	appendN(t, l, 3*indexEvery+5)
	l.Close()
	r, err := Open(l.Path())
	if err != nil {
		t.Fatal(err)
	}
	if r.Last() != 3*indexEvery+5 {
		t.Fatalf("Last() = %d", r.Last())
	}
	checkSeqs(t, readAll(t, r, 130), 130, 3*indexEvery+5)
	if _, err := r.Append(api.Event{Type: api.EventStdout}); !errors.Is(err, ErrClosed) {
		t.Fatalf("append to opened log: %v", err)
	}
}

func TestFollowWaitsForAppendsAndClose(t *testing.T) {
	l := newLog(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	var got []api.Event
	var finalErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		from := uint64(1)
		for {
			evs, err := l.Read(ctx, from, 10, true)
			if err != nil {
				finalErr = err
				return
			}
			got = append(got, evs...)
			from = evs[len(evs)-1].Seq + 1
		}
	}()
	for i := 0; i < 50; i++ {
		if _, err := l.Append(api.Event{Type: api.EventStdout, Data: []byte(fmt.Sprintf("line %d\n", i))}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Millisecond)
	}
	l.Close()
	wg.Wait()
	if finalErr != io.EOF {
		t.Fatalf("follow ended with %v, want io.EOF", finalErr)
	}
	checkSeqs(t, got, 1, 50)
}

func TestFollowHonoursContext(t *testing.T) {
	l := newLog(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := l.Read(ctx, 1, 10, true); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
}
