package server

import (
	"context"
	"io"
	"unicode/utf8"

	"github.com/codemug/shhttp/v2/internal/eventlog"
	"github.com/codemug/shhttp/v2/pkg/api"
)

// collectOutput gathers the stdout and stderr written so far, keeping at
// most max bytes of each.
func collectOutput(ctx context.Context, l *eventlog.Log, max int, keepTail bool) (api.Output, error) {
	stdout := &capped{max: max, tail: keepTail}
	stderr := &capped{max: max, tail: keepTail}
	from := uint64(1)
	for {
		evs, err := l.Read(ctx, from, 1024, false)
		if err == io.EOF {
			break
		}
		if err != nil {
			return api.Output{}, err
		}
		for _, e := range evs {
			switch e.Type {
			case api.EventStdout:
				stdout.add(e.Data)
			case api.EventStderr:
				stderr.add(e.Data)
			}
		}
		from = evs[len(evs)-1].Seq + 1
	}
	var out api.Output
	out.SetStdout(stdout.result())
	out.SetStderr(stderr.result())
	return out, nil
}

// capped keeps the first or last max bytes written to it.
type capped struct {
	buf       []byte
	max       int
	tail      bool
	truncated bool
}

func (c *capped) add(b []byte) {
	if !c.tail {
		room := c.max - len(c.buf)
		if len(b) > room {
			b = b[:max(room, 0)]
			c.truncated = true
		}
		c.buf = append(c.buf, b...)
		return
	}
	c.buf = append(c.buf, b...)
	if len(c.buf) > 2*c.max {
		c.buf = append([]byte(nil), c.buf[len(c.buf)-c.max:]...)
		c.truncated = true
	}
}

// result returns the kept bytes, trimmed so that truncation does not split a
// UTF-8 character.
func (c *capped) result() ([]byte, bool) {
	b := c.buf
	if c.tail && len(b) > c.max {
		b = b[len(b)-c.max:]
		c.truncated = true
	}
	if !c.truncated || !utf8.Valid(c.buf) {
		return b, c.truncated
	}
	if c.tail {
		for i := 0; i < utf8.UTFMax && len(b) > 0 && !utf8.RuneStart(b[0]); i++ {
			b = b[1:]
		}
	} else {
		for i := 0; i < utf8.UTFMax && len(b) > 0 && !utf8.Valid(b); i++ {
			b = b[:len(b)-1]
		}
	}
	return b, true
}
