package client

import (
	"context"
	"iter"
	"net/http"
	"net/url"
	"strconv"

	"github.com/codemug/shhttp/pkg/api"
)

// SubmitJob submits a job (scope jobs:run).
func (c *Client) SubmitJob(ctx context.Context, spec api.JobSpec) (api.Job, error) {
	var j api.Job
	return j, c.call(ctx, http.MethodPost, "/v2/jobs", nil, spec, &j)
}

// GetJob returns a job.
func (c *Client) GetJob(ctx context.Context, id string) (api.Job, error) {
	var j api.Job
	return j, c.call(ctx, http.MethodGet, "/v2/jobs/"+url.PathEscape(id), nil, nil, &j)
}

// JobListOptions filter ListJobs.
type JobListOptions struct {
	State  api.JobState
	Queue  string
	KeyID  string
	Labels map[string]string
	Limit  int
	Cursor string
}

// ListJobs returns a page of jobs, newest first.
func (c *Client) ListJobs(ctx context.Context, opts *JobListOptions) (api.JobList, error) {
	q := url.Values{}
	if opts != nil {
		if opts.State != "" {
			q.Set("state", string(opts.State))
		}
		if opts.Queue != "" {
			q.Set("queue", opts.Queue)
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
	var l api.JobList
	return l, c.call(ctx, http.MethodGet, "/v2/jobs", q, nil, &l)
}

// CancelJob cancels a job and returns it once it has ended.
func (c *Client) CancelJob(ctx context.Context, id string) (api.Job, error) {
	var j api.Job
	return j, c.call(ctx, http.MethodPost, "/v2/jobs/"+url.PathEscape(id)+"/cancel", nil, nil, &j)
}

// DeleteJob cancels the job if needed and deletes it and its step sessions.
func (c *Client) DeleteJob(ctx context.Context, id string) error {
	return c.call(ctx, http.MethodDelete, "/v2/jobs/"+url.PathEscape(id), nil, nil, nil)
}

// JobEvents yields a job's events: job_started, step_started,
// step_finished and job_finished.
func (c *Client) JobEvents(ctx context.Context, id string, opts *EventsOptions) iter.Seq2[api.Event, error] {
	return c.events(ctx, "/v2/jobs/"+url.PathEscape(id)+"/events", opts)
}

// Queues lists queues with their load.
func (c *Client) Queues(ctx context.Context) ([]api.Queue, error) {
	var l api.QueueList
	return l.Queues, c.call(ctx, http.MethodGet, "/v2/queues", nil, nil, &l)
}

// PutQueue creates or resizes a queue (scope queues:write).
func (c *Client) PutQueue(ctx context.Context, name string, concurrency int) (api.Queue, error) {
	var q api.Queue
	return q, c.call(ctx, http.MethodPut, "/v2/queues/"+url.PathEscape(name), nil, api.PutQueueRequest{Concurrency: concurrency}, &q)
}

// DeleteQueue deletes an empty queue.
func (c *Client) DeleteQueue(ctx context.Context, name string) error {
	return c.call(ctx, http.MethodDelete, "/v2/queues/"+url.PathEscape(name), nil, nil, nil)
}

// ListTemplates returns every template.
func (c *Client) ListTemplates(ctx context.Context) ([]api.Template, error) {
	var l api.TemplateList
	return l.Templates, c.call(ctx, http.MethodGet, "/v2/templates", nil, nil, &l)
}

// GetTemplate returns a template.
func (c *Client) GetTemplate(ctx context.Context, name string) (api.Template, error) {
	var t api.Template
	return t, c.call(ctx, http.MethodGet, "/v2/templates/"+url.PathEscape(name), nil, nil, &t)
}

// PutTemplate creates or replaces a template (scope templates:write).
func (c *Client) PutTemplate(ctx context.Context, name string, spec api.TemplateSpec) (api.Template, error) {
	var t api.Template
	return t, c.call(ctx, http.MethodPut, "/v2/templates/"+url.PathEscape(name), nil, spec, &t)
}

// DeleteTemplate deletes a template.
func (c *Client) DeleteTemplate(ctx context.Context, name string) error {
	return c.call(ctx, http.MethodDelete, "/v2/templates/"+url.PathEscape(name), nil, nil, nil)
}

// RunTemplate starts a template's session or submits its job (scope
// templates:run).
func (c *Client) RunTemplate(ctx context.Context, name string, params map[string]string) (api.RunTemplateResponse, error) {
	var r api.RunTemplateResponse
	return r, c.call(ctx, http.MethodPost, "/v2/templates/"+url.PathEscape(name)+"/run", nil, api.RunTemplateRequest{Params: params}, &r)
}
