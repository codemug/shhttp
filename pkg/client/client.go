// Package client is a Go client for the shhttp v2 API.
//
//	c := client.New("http://127.0.0.1:2112", os.Getenv("SHHTTP_KEY"))
//	res, err := c.Run(ctx, api.SessionSpec{Argv: []string{"uname", "-a"}}, nil)
//
// Errors returned by the server are *api.Problem values; use errors.As to
// inspect the status.
package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/codemug/shhttp/pkg/api"
)

// Client calls one shhttp server with one key.
type Client struct {
	baseURL string
	key     string
	http    *http.Client
}

// Option configures a Client.
type Option func(*Client)

// WithHTTPClient sets the HTTP client used for requests and WebSocket
// handshakes. The default is http.DefaultClient.
func WithHTTPClient(h *http.Client) Option { return func(c *Client) { c.http = h } }

// New returns a client for the server at baseURL (for example
// "http://127.0.0.1:2112") that authenticates with key, an API key or the
// master key.
func New(baseURL, key string, opts ...Option) *Client {
	c := &Client{baseURL: strings.TrimRight(baseURL, "/"), key: key, http: http.DefaultClient}
	for _, o := range opts {
		o(c)
	}
	return c
}

// BaseURL returns the server URL the client was created with.
func (c *Client) BaseURL() string { return c.baseURL }

// rawBody marks a request body that is sent as bytes, not JSON.
type rawBody struct{ io.Reader }

func (c *Client) newRequest(ctx context.Context, method, path string, query url.Values, body any) (*http.Request, error) {
	u := c.baseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var r io.Reader
	contentType := ""
	switch b := body.(type) {
	case nil:
	case rawBody:
		r, contentType = b.Reader, "application/octet-stream"
	default:
		j, err := json.Marshal(b)
		if err != nil {
			return nil, err
		}
		r, contentType = bytes.NewReader(j), "application/json"
	}
	req, err := http.NewRequestWithContext(ctx, method, u, r)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if c.key != "" {
		req.Header.Set("Authorization", "Bearer "+c.key)
	}
	return req, nil
}

// send performs a request and returns the response if its status is below
// 400; otherwise it returns the server's problem as an error.
func (c *Client) send(req *http.Request) (*http.Response, error) {
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		return nil, problemFrom(resp.StatusCode, resp.Body)
	}
	return resp, nil
}

func problemFrom(status int, body io.Reader) *api.Problem {
	var p api.Problem
	b, _ := io.ReadAll(io.LimitReader(body, 1<<20))
	if json.Unmarshal(b, &p) != nil || p.Status == 0 {
		p = api.Problem{Status: status, Title: http.StatusText(status), Detail: strings.TrimSpace(string(b))}
	}
	return &p
}

// call sends a request and decodes a JSON response into out (if not nil).
func (c *Client) call(ctx context.Context, method, path string, query url.Values, body, out any) error {
	req, err := c.newRequest(ctx, method, path, query, body)
	if err != nil {
		return err
	}
	resp, err := c.send(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		_, err = io.Copy(io.Discard, resp.Body)
		return err
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// IsStatus reports whether err is a server problem with the given status.
func IsStatus(err error, status int) bool {
	var p *api.Problem
	return errors.As(err, &p) && p.Status == status
}

// Create starts a session and returns at once.
func (c *Client) Create(ctx context.Context, spec api.SessionSpec) (api.Session, error) {
	var s api.Session
	return s, c.call(ctx, http.MethodPost, "/v2/sessions", nil, spec, &s)
}

// RunOptions tune Run. The zero value waits until the session ends and
// keeps the last 1 MiB of each stream.
type RunOptions struct {
	// WaitTimeout makes Run return early with the session still running.
	WaitTimeout time.Duration
	// MaxOutput is the most bytes of each stream to return.
	MaxOutput int
	// KeepHead keeps the start of truncated output instead of the end.
	KeepHead bool
}

// Run starts a session, waits for it to end and returns its output. Stdin
// is closed after spec's initial input.
func (c *Client) Run(ctx context.Context, spec api.SessionSpec, opts *RunOptions) (api.RunResult, error) {
	q := url.Values{"wait": {"true"}}
	if opts != nil {
		if opts.WaitTimeout > 0 {
			q.Set("wait_timeout", opts.WaitTimeout.String())
		}
		if opts.MaxOutput > 0 {
			q.Set("max_output", strconv.Itoa(opts.MaxOutput))
		}
		if opts.KeepHead {
			q.Set("keep", "head")
		}
	}
	var r api.RunResult
	return r, c.call(ctx, http.MethodPost, "/v2/sessions", q, spec, &r)
}

// ListOptions filter List.
type ListOptions struct {
	State  api.SessionState
	KeyID  string
	Labels map[string]string
	Limit  int
	Cursor string
}

// List returns a page of sessions, newest first.
func (c *Client) List(ctx context.Context, opts *ListOptions) (api.SessionList, error) {
	q := url.Values{}
	if opts != nil {
		if opts.State != "" {
			q.Set("state", string(opts.State))
		}
		if opts.KeyID != "" {
			q.Set("key_id", opts.KeyID)
		}
		for k, v := range opts.Labels {
			q.Add("label", k+"="+v)
		}
		if opts.Limit > 0 {
			q.Set("limit", strconv.Itoa(opts.Limit))
		}
		if opts.Cursor != "" {
			q.Set("cursor", opts.Cursor)
		}
	}
	var l api.SessionList
	return l, c.call(ctx, http.MethodGet, "/v2/sessions", q, nil, &l)
}

// Get returns a session.
func (c *Client) Get(ctx context.Context, id string) (api.Session, error) {
	var s api.Session
	return s, c.call(ctx, http.MethodGet, "/v2/sessions/"+url.PathEscape(id), nil, nil, &s)
}

// EventsOptions tune Events.
type EventsOptions struct {
	// From is the first sequence number to return (default 1).
	From uint64
	// Follow keeps streaming until the session ends.
	Follow bool
}

// Events yields a session's events. Stop ranging, or cancel ctx, to end the
// request early. A failure ends the sequence with a non-nil error.
func (c *Client) Events(ctx context.Context, id string, opts *EventsOptions) iter.Seq2[api.Event, error] {
	return c.events(ctx, "/v2/sessions/"+url.PathEscape(id)+"/events", opts)
}

func (c *Client) events(ctx context.Context, path string, opts *EventsOptions) iter.Seq2[api.Event, error] {
	return func(yield func(api.Event, error) bool) {
		q := url.Values{}
		if opts != nil {
			if opts.From > 0 {
				q.Set("from", strconv.FormatUint(opts.From, 10))
			}
			if opts.Follow {
				q.Set("follow", "true")
			}
		}
		req, err := c.newRequest(ctx, http.MethodGet, path, q, nil)
		if err != nil {
			yield(api.Event{}, err)
			return
		}
		req.Header.Set("Accept", "application/x-ndjson")
		resp, err := c.send(req)
		if err != nil {
			yield(api.Event{}, err)
			return
		}
		defer resp.Body.Close()
		r := bufio.NewReader(resp.Body)
		for {
			line, err := r.ReadBytes('\n')
			if len(bytes.TrimSpace(line)) > 0 {
				var e api.Event
				if jerr := json.Unmarshal(line, &e); jerr != nil {
					yield(api.Event{}, fmt.Errorf("decoding event: %w", jerr))
					return
				}
				if !yield(e, nil) {
					return
				}
			}
			if err == io.EOF {
				return
			}
			if err != nil {
				yield(api.Event{}, err)
				return
			}
		}
	}
}

// WriteStdin streams r into the session's stdin, closing it afterwards if
// closeAfter is set. r may be nil.
func (c *Client) WriteStdin(ctx context.Context, id string, r io.Reader, closeAfter bool) (api.StdinResponse, error) {
	if r == nil {
		r = bytes.NewReader(nil)
	}
	var q url.Values
	if closeAfter {
		q = url.Values{"close": {"true"}}
	}
	var out api.StdinResponse
	return out, c.call(ctx, http.MethodPost, "/v2/sessions/"+url.PathEscape(id)+"/stdin", q, rawBody{r}, &out)
}

// CloseStdin closes the session's stdin.
func (c *Client) CloseStdin(ctx context.Context, id string) error {
	_, err := c.WriteStdin(ctx, id, nil, true)
	return err
}

// Signal sends a signal, such as "SIGINT", to the session's process group.
func (c *Client) Signal(ctx context.Context, id, signal string) (api.Session, error) {
	var s api.Session
	return s, c.call(ctx, http.MethodPost, "/v2/sessions/"+url.PathEscape(id)+"/signal", nil, api.SignalRequest{Signal: signal}, &s)
}

// Kill stops the session: SIGTERM, then SIGKILL after the server's grace
// period.
func (c *Client) Kill(ctx context.Context, id string) (api.Session, error) {
	var s api.Session
	return s, c.call(ctx, http.MethodPost, "/v2/sessions/"+url.PathEscape(id)+"/kill", nil, nil, &s)
}

// Delete kills the session if needed and deletes it and its output.
func (c *Client) Delete(ctx context.Context, id string) error {
	return c.call(ctx, http.MethodDelete, "/v2/sessions/"+url.PathEscape(id), nil, nil, nil)
}

// Whoami describes the client's key.
func (c *Client) Whoami(ctx context.Context) (api.Whoami, error) {
	var w api.Whoami
	return w, c.call(ctx, http.MethodGet, "/v2/whoami", nil, nil, &w)
}

// Version returns the server's version.
func (c *Client) Version(ctx context.Context) (string, error) {
	var v api.Version
	return v.Version, c.call(ctx, http.MethodGet, "/v2/version", nil, nil, &v)
}

// The key methods need the master key.

// CreateKey creates an API key. The result holds the full key, shown once.
func (c *Client) CreateKey(ctx context.Context, req api.CreateKeyRequest) (api.KeyWithSecret, error) {
	var k api.KeyWithSecret
	return k, c.call(ctx, http.MethodPost, "/v2/keys", nil, req, &k)
}

// ListKeys returns every key, including revoked ones.
func (c *Client) ListKeys(ctx context.Context) ([]api.Key, error) {
	var l api.KeyList
	return l.Keys, c.call(ctx, http.MethodGet, "/v2/keys", nil, nil, &l)
}

// GetKey returns a key.
func (c *Client) GetKey(ctx context.Context, id string) (api.Key, error) {
	var k api.Key
	return k, c.call(ctx, http.MethodGet, "/v2/keys/"+url.PathEscape(id), nil, nil, &k)
}

// UpdateKey changes a key; nil fields are left unchanged.
func (c *Client) UpdateKey(ctx context.Context, id string, req api.UpdateKeyRequest) (api.Key, error) {
	var k api.Key
	return k, c.call(ctx, http.MethodPatch, "/v2/keys/"+url.PathEscape(id), nil, req, &k)
}

// RotateKey issues a new secret. With grace > 0 the old one keeps working
// that long.
func (c *Client) RotateKey(ctx context.Context, id string, grace time.Duration) (api.KeyWithSecret, error) {
	var k api.KeyWithSecret
	return k, c.call(ctx, http.MethodPost, "/v2/keys/"+url.PathEscape(id)+"/rotate", nil, api.RotateKeyRequest{Grace: api.Duration(grace)}, &k)
}

// RevokeKey permanently disables a key, optionally killing its running
// sessions.
func (c *Client) RevokeKey(ctx context.Context, id string, killSessions bool) (api.RevokeKeyResponse, error) {
	var q url.Values
	if killSessions {
		q = url.Values{"kill_sessions": {"true"}}
	}
	var r api.RevokeKeyResponse
	return r, c.call(ctx, http.MethodDelete, "/v2/keys/"+url.PathEscape(id), q, nil, &r)
}
