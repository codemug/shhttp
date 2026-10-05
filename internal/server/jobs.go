package server

import (
	"context"
	"net/http"
	"slices"
	"strings"

	"github.com/codemug/shhttp/v2/internal/id"
	"github.com/codemug/shhttp/v2/internal/session"
	"github.com/codemug/shhttp/v2/internal/store"
	"github.com/codemug/shhttp/v2/internal/template"
	"github.com/codemug/shhttp/v2/pkg/api"
	"github.com/danielgtaylor/huma/v2"
)

// JobPath is the {id} of job operations (exported for embedding, as
// KeyPath).
type JobPath struct {
	ID string `path:"id" doc:"Job id" example:"job_01j9z3k5q8m2x7v4w6t0b1c3d5"`
}

type submitJobInput struct {
	Body api.JobSpec
}

type jobOutput struct {
	Location string `header:"Location"`
	Body     api.Job
}

type listJobsInput struct {
	State  string   `query:"state" enum:"queued,running,succeeded,failed,cancelled,lost" doc:"Only jobs in this state."`
	Queue  string   `query:"queue" doc:"Only jobs of this queue."`
	KeyID  string   `query:"key_id" doc:"Only jobs of this key. Other keys' jobs need the admin:read scope."`
	Label  []string `query:"label,explode" doc:"Only jobs with this label, written key=value."`
	Limit  int      `query:"limit" minimum:"1" maximum:"500" default:"50"`
	Cursor string   `query:"cursor"`
}

type jobListOutput struct{ Body api.JobList }

type jobEventsInput struct {
	JobPath
	From        uint64 `query:"from" default:"1"`
	Follow      bool   `query:"follow" doc:"Keep streaming until the job ends."`
	Format      string `query:"format" enum:"ndjson,sse"`
	Accept      string `header:"Accept"`
	LastEventID string `header:"Last-Event-ID"`
}

// QueuePath is exported for embedding (see KeyPath).
type QueuePath struct {
	Name string `path:"name" doc:"Queue name" example:"deploys"`
}

type putQueueInput struct {
	QueuePath
	Body api.PutQueueRequest
}

type queueOutput struct{ Body api.Queue }

type queueListOutput struct{ Body api.QueueList }

// TemplatePath is exported for embedding (see KeyPath).
type TemplatePath struct {
	Name string `path:"name" doc:"Template name" example:"deploy"`
}

type putTemplateInput struct {
	TemplatePath
	Body api.TemplateSpec
}

type templateOutput struct{ Body api.Template }

type templateListOutput struct{ Body api.TemplateList }

type runTemplateInput struct {
	TemplatePath
	Body *api.RunTemplateRequest `required:"false"`
}

type runTemplateOutput struct {
	Location string `header:"Location"`
	Body     api.RunTemplateResponse
}

// loadJob returns the job if the caller may see it (or, with write set,
// change it); otherwise 404.
func (s *Server) loadJob(ctx context.Context, jid string, write bool) (api.Job, error) {
	if !id.Valid(id.Job, jid) {
		return api.Job{}, huma.Error404NotFound("not found")
	}
	j, err := s.jobs.Get(ctx, jid)
	if err != nil {
		return api.Job{}, s.apiError(err)
	}
	p := principalFrom(ctx)
	if (write && j.KeyID != p.KeyID()) || (j.KeyID != p.KeyID() && !p.Has(api.ScopeAdminRead)) {
		return api.Job{}, huma.Error404NotFound("not found")
	}
	return j, nil
}

func (s *Server) registerJobs() {
	run := access{scope: api.ScopeJobsRun}
	read := access{scope: api.ScopeJobsRead}

	op := operation("submit-job", http.MethodPost, "/v2/jobs", "Submit a job", "Jobs", run)
	op.Description = "Steps run one after another unless any step sets depends_on, in which case they form a dependency graph " +
		"and independent steps run in parallel. Every step is checked against the key's policy at submission. " +
		"With a queue the job waits its turn; without one it starts at once. Each step runs as a session labelled " +
		"shhttp.job and shhttp.step; read its output through the session endpoints (sessions:read)."
	op.DefaultStatus = http.StatusCreated
	op.MaxBodyBytes = maxSessionBody
	op.Errors = append(op.Errors, http.StatusBadRequest, http.StatusServiceUnavailable)
	huma.Register(s.api, op, func(ctx context.Context, in *submitJobInput) (*jobOutput, error) {
		j, err := s.jobs.Submit(ctx, principalFrom(ctx).KeyID(), in.Body)
		if err != nil {
			return nil, s.apiError(err)
		}
		return &jobOutput{Location: "/v2/jobs/" + j.ID, Body: j}, nil
	})

	op = operation("list-jobs", http.MethodGet, "/v2/jobs", "List jobs", "Jobs", read)
	op.Description = "Newest first. A key sees its own jobs; admin:read sees every key's."
	huma.Register(s.api, op, func(ctx context.Context, in *listJobsInput) (*jobListOutput, error) {
		p := principalFrom(ctx)
		f := store.JobFilter{KeyID: p.KeyID(), Limit: in.Limit}
		if f.Limit == 0 {
			f.Limit = defaultListLimit
		}
		if p.Has(api.ScopeAdminRead) {
			f.KeyID = in.KeyID
		} else if in.KeyID != "" && in.KeyID != p.KeyID() {
			return nil, huma.Error403Forbidden("listing another key's jobs needs the admin:read scope")
		}
		if in.State != "" {
			f.States = []api.JobState{api.JobState(in.State)}
		}
		if in.Queue != "" {
			f.Queue = &in.Queue
		}
		for _, l := range in.Label {
			k, v, ok := strings.Cut(l, "=")
			if !ok || k == "" {
				return nil, huma.Error400BadRequest("label filters look like label=key=value")
			}
			if f.Labels == nil {
				f.Labels = map[string]string{}
			}
			f.Labels[k] = v
		}
		if in.Cursor != "" {
			if !id.Valid(id.Job, in.Cursor) {
				return nil, huma.Error400BadRequest("invalid cursor")
			}
			f.Before = in.Cursor
		}
		jobs, err := s.jobs.List(ctx, f)
		if err != nil {
			return nil, s.apiError(err)
		}
		out := api.JobList{Jobs: jobs}
		if len(jobs) == f.Limit {
			out.NextCursor = jobs[len(jobs)-1].ID
		}
		return &jobListOutput{Body: out}, nil
	})

	op = operation("get-job", http.MethodGet, "/v2/jobs/{id}", "Get a job", "Jobs", read)
	op.Errors = append(op.Errors, http.StatusNotFound)
	huma.Register(s.api, op, func(ctx context.Context, in *JobPath) (*jobOutput, error) {
		j, err := s.loadJob(ctx, in.ID, false)
		if err != nil {
			return nil, err
		}
		return &jobOutput{Body: j}, nil
	})

	op = operation("cancel-job", http.MethodPost, "/v2/jobs/{id}/cancel", "Cancel a job", "Jobs", run)
	op.Description = "Kills the job's running steps and skips the rest. Returns once the job has ended."
	op.Errors = append(op.Errors, http.StatusNotFound, http.StatusConflict)
	huma.Register(s.api, op, func(ctx context.Context, in *JobPath) (*jobOutput, error) {
		if _, err := s.loadJob(ctx, in.ID, true); err != nil {
			return nil, err
		}
		j, err := s.jobs.Cancel(ctx, in.ID)
		if err != nil {
			return nil, s.apiError(err)
		}
		return &jobOutput{Body: j}, nil
	})

	op = operation("delete-job", http.MethodDelete, "/v2/jobs/{id}", "Delete a job", "Jobs", run)
	op.Description = "Cancels the job if needed, then deletes it, its events and its step sessions."
	op.Errors = append(op.Errors, http.StatusNotFound)
	huma.Register(s.api, op, func(ctx context.Context, in *JobPath) (*struct{}, error) {
		if _, err := s.loadJob(ctx, in.ID, true); err != nil {
			return nil, err
		}
		return nil, s.mapNil(s.jobs.Delete(ctx, in.ID))
	})

	op = operation("get-job-events", http.MethodGet, "/v2/jobs/{id}/events", "Read or follow a job's events", "Jobs", read)
	op.Description = "Job events: job_started, step_started (with session_id), step_finished (with state, exit_code, error) and job_finished. " +
		"Same formats and resumption as session events."
	op.Errors = append(op.Errors, http.StatusNotFound)
	op.Responses = map[string]*huma.Response{"200": {
		Description: "The events.",
		Content: map[string]*huma.MediaType{
			"application/x-ndjson": {Schema: s.api.OpenAPI().Components.Schemas.Schema(reflectEvent, true, "Event")},
			"text/event-stream":    {Schema: &huma.Schema{Type: "string"}},
		},
	}}
	huma.Register(s.api, op, func(ctx context.Context, in *jobEventsInput) (*streamOutput, error) {
		j, err := s.loadJob(ctx, in.ID, false)
		if err != nil {
			return nil, err
		}
		from, err := resumeFrom(in.From, in.LastEventID)
		if err != nil {
			return nil, err
		}
		format := formatNDJSON
		if in.Format == "sse" || (in.Format == "" && strings.Contains(in.Accept, "text/event-stream")) {
			format = formatSSE
		}
		l, err := s.jobs.Log(ctx, j.ID)
		if err != nil {
			return nil, s.apiError(err)
		}
		return &streamOutput{Body: func(hctx huma.Context) {
			s.streamEvents(hctx, l, j.ID, from, in.Follow, format, "", false)
		}}, nil
	})

	op = operation("list-queues", http.MethodGet, "/v2/queues", "List queues", "Queues", read)
	huma.Register(s.api, op, func(ctx context.Context, _ *struct{}) (*queueListOutput, error) {
		qs, err := s.jobs.Queues(ctx)
		if err != nil {
			return nil, s.apiError(err)
		}
		return &queueListOutput{Body: api.QueueList{Queues: qs}}, nil
	})

	op = operation("put-queue", http.MethodPut, "/v2/queues/{name}", "Create or resize a queue", "Queues", access{scope: api.ScopeQueuesWrite})
	op.MaxBodyBytes = maxJSONBody
	op.Errors = append(op.Errors, http.StatusBadRequest)
	huma.Register(s.api, op, func(ctx context.Context, in *putQueueInput) (*queueOutput, error) {
		if err := s.jobs.PutQueue(ctx, in.Name, in.Body.Concurrency); err != nil {
			return nil, s.apiError(err)
		}
		qs, err := s.jobs.Queues(ctx)
		if err != nil {
			return nil, s.apiError(err)
		}
		i := slices.IndexFunc(qs, func(q api.Queue) bool { return q.Name == in.Name })
		s.log.Info("queue updated", "audit", true, "queue", in.Name, "concurrency", in.Body.Concurrency, "key", principalFrom(ctx).KeyID())
		return &queueOutput{Body: qs[i]}, nil
	})

	op = operation("delete-queue", http.MethodDelete, "/v2/queues/{name}", "Delete a queue", "Queues", access{scope: api.ScopeQueuesWrite})
	op.Description = "Only empty queues can be deleted, and never the default queue."
	op.Errors = append(op.Errors, http.StatusBadRequest, http.StatusNotFound, http.StatusConflict)
	huma.Register(s.api, op, func(ctx context.Context, in *QueuePath) (*struct{}, error) {
		return nil, s.mapNil(s.jobs.DeleteQueue(ctx, in.Name))
	})
}

func (s *Server) registerTemplates() {
	readers := access{anyOf: []string{api.ScopeTemplatesRead, api.ScopeTemplatesRun}}
	writers := access{scope: api.ScopeTemplatesWrite}

	op := operation("list-templates", http.MethodGet, "/v2/templates", "List templates", "Templates", readers)
	huma.Register(s.api, op, func(ctx context.Context, _ *struct{}) (*templateListOutput, error) {
		ts, err := s.templates.List(ctx)
		if err != nil {
			return nil, s.apiError(err)
		}
		return &templateListOutput{Body: api.TemplateList{Templates: ts}}, nil
	})

	op = operation("get-template", http.MethodGet, "/v2/templates/{name}", "Get a template", "Templates", readers)
	op.Errors = append(op.Errors, http.StatusNotFound)
	huma.Register(s.api, op, func(ctx context.Context, in *TemplatePath) (*templateOutput, error) {
		t, err := s.templates.Get(ctx, in.Name)
		if err != nil {
			return nil, s.apiError(err)
		}
		return &templateOutput{Body: t}, nil
	})

	op = operation("put-template", http.MethodPut, "/v2/templates/{name}", "Create or replace a template", "Templates", writers)
	op.Description = "A template is a session or job spec with typed parameters. {{name}} placeholders are replaced in argv " +
		"elements, env values and cwd, never in shell command lines; every rendered session also gets each parameter as " +
		"SHHTTP_PARAM_<NAME>, which is how shell command lines should use them."
	op.MaxBodyBytes = maxSessionBody
	op.Errors = append(op.Errors, http.StatusBadRequest)
	huma.Register(s.api, op, func(ctx context.Context, in *putTemplateInput) (*templateOutput, error) {
		t, err := s.templates.Put(ctx, in.Name, in.Body, principalFrom(ctx).KeyID())
		if err != nil {
			return nil, s.apiError(err)
		}
		s.log.Info("template saved", "audit", true, "template", in.Name, "key", principalFrom(ctx).KeyID())
		return &templateOutput{Body: t}, nil
	})

	op = operation("delete-template", http.MethodDelete, "/v2/templates/{name}", "Delete a template", "Templates", writers)
	op.Errors = append(op.Errors, http.StatusNotFound)
	huma.Register(s.api, op, func(ctx context.Context, in *TemplatePath) (*struct{}, error) {
		if err := s.templates.Delete(ctx, in.Name); err != nil {
			return nil, s.apiError(err)
		}
		s.log.Info("template deleted", "audit", true, "template", in.Name, "key", principalFrom(ctx).KeyID())
		return nil, nil
	})

	op = operation("run-template", http.MethodPost, "/v2/templates/{name}/run", "Run a template", "Templates", access{scope: api.ScopeTemplatesRun})
	op.Description = "Fills in the parameters and starts the session or submits the job, under the calling key and its policy. " +
		"A key whose policy lists templates may only run those."
	op.DefaultStatus = http.StatusCreated
	op.MaxBodyBytes = maxJSONBody
	op.Errors = append(op.Errors, http.StatusBadRequest, http.StatusNotFound, http.StatusTooManyRequests)
	huma.Register(s.api, op, func(ctx context.Context, in *runTemplateInput) (*runTemplateOutput, error) {
		p := principalFrom(ctx)
		if allowed := p.Key.Policy.Templates; len(allowed) > 0 && !slices.Contains(allowed, in.Name) {
			return nil, huma.Error403Forbidden("this key may not run template " + in.Name)
		}
		t, err := s.templates.Get(ctx, in.Name)
		if err != nil {
			return nil, s.apiError(err)
		}
		var params map[string]string
		if in.Body != nil {
			params = in.Body.Params
		}
		sessSpec, jobSpec, err := template.Render(t, params)
		if err != nil {
			return nil, s.apiError(err)
		}
		if sessSpec != nil {
			sess, err := s.sessions.Create(ctx, session.Owner{KeyID: p.KeyID(), Policy: p.Key.Policy}, *sessSpec)
			if err != nil {
				return nil, s.apiError(err)
			}
			return &runTemplateOutput{Location: "/v2/sessions/" + sess.ID, Body: api.RunTemplateResponse{Session: &sess}}, nil
		}
		j, err := s.jobs.Submit(ctx, p.KeyID(), *jobSpec)
		if err != nil {
			return nil, s.apiError(err)
		}
		return &runTemplateOutput{Location: "/v2/jobs/" + j.ID, Body: api.RunTemplateResponse{Job: &j}}, nil
	})
}

// mapNil passes nil through and maps other errors.
func (s *Server) mapNil(err error) error {
	if err == nil {
		return nil
	}
	return s.apiError(err)
}
