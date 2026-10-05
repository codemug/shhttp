package api

import "time"

// JobStep is one step of a job; it runs as a session.
type JobStep struct {
	// Name identifies the step within the job. Defaults to "step-<n>".
	Name string      `json:"name,omitempty" pattern:"^[A-Za-z0-9_.-]{1,64}$" example:"build"`
	Spec SessionSpec `json:"spec"`
	// DependsOn lists steps that must succeed first. When no step of a job
	// has depends_on, steps run one after another in order.
	DependsOn []string `json:"depends_on,omitempty" doc:"Steps that must succeed first. If no step sets this, steps run in order."`
	// AllowFailure lets dependents and the job continue when this step fails.
	AllowFailure bool `json:"allow_failure,omitempty" doc:"A failure of this step does not fail the job or skip its dependents."`
}

// JobSpec describes a job.
type JobSpec struct {
	Name string `json:"name,omitempty" maxLength:"100" example:"deploy"`
	// Queue runs the job through a named queue. Without one it starts at once.
	Queue string    `json:"queue,omitempty" doc:"Run through this queue; omit to start at once." example:"default"`
	Steps []JobStep `json:"steps" minItems:"1" maxItems:"100"`
	// Env is merged into every step's environment; a step's own env wins.
	Env map[string]string `json:"env,omitempty" doc:"Added to every step's environment; a step's own env wins."`
	// OnRestart says what happens to the job if the server stops while it
	// runs: "fail" (default) or "resume", which re-runs interrupted steps.
	OnRestart string            `json:"on_restart,omitempty" enum:"fail,resume" doc:"After a server restart: fail the job (default) or re-run the interrupted steps and continue."`
	Labels    map[string]string `json:"labels,omitempty"`
	// Retention is how long the job is kept after it ends. Its step
	// sessions follow their own retention.
	Retention Duration `json:"retention,omitempty" doc:"Keep the job this long after it ends. Defaults to the server setting." example:"24h"`
}

// JobState is the lifecycle state of a job.
type JobState string

const (
	JobQueued    JobState = "queued"
	JobRunning   JobState = "running"
	JobSucceeded JobState = "succeeded"
	JobFailed    JobState = "failed"
	JobCancelled JobState = "cancelled"
	JobLost      JobState = "lost"
)

// Finished reports whether the state is terminal.
func (s JobState) Finished() bool { return s != JobQueued && s != JobRunning }

// StepState is the state of a job step.
type StepState string

const (
	StepPending   StepState = "pending"
	StepRunning   StepState = "running"
	StepSucceeded StepState = "succeeded"
	StepFailed    StepState = "failed"
	StepSkipped   StepState = "skipped"
	StepCancelled StepState = "cancelled"
)

// StepStatus is a step's progress.
type StepStatus struct {
	Name      string     `json:"name"`
	State     StepState  `json:"state" enum:"pending,running,succeeded,failed,skipped,cancelled"`
	SessionID string     `json:"session_id,omitempty" doc:"The step's session; read its output through the session endpoints."`
	ExitCode  *int       `json:"exit_code,omitempty"`
	Signal    string     `json:"signal,omitempty"`
	Error     string     `json:"error,omitempty"`
	StartedAt *time.Time `json:"started_at,omitempty"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
}

// Job is a job's metadata and progress.
type Job struct {
	ID        string       `json:"id"`
	KeyID     string       `json:"key_id"`
	Spec      JobSpec      `json:"spec"`
	State     JobState     `json:"state" enum:"queued,running,succeeded,failed,cancelled,lost"`
	Steps     []StepStatus `json:"steps"`
	Error     string       `json:"error,omitempty"`
	CreatedAt time.Time    `json:"created_at"`
	StartedAt *time.Time   `json:"started_at,omitempty"`
	EndedAt   *time.Time   `json:"ended_at,omitempty"`
	ExpiresAt *time.Time   `json:"expires_at,omitempty"`
}

// JobList is a page of jobs.
type JobList struct {
	Jobs       []Job  `json:"jobs"`
	NextCursor string `json:"next_cursor,omitempty"`
}

// Job event types, written to the job's event log. They use the Event
// fields State, ExitCode, Signal, Error, Step and SessionID.
const (
	EventJobStarted   EventType = "job_started"
	EventStepStarted  EventType = "step_started"
	EventStepFinished EventType = "step_finished"
	EventJobFinished  EventType = "job_finished"
)

// Queue is a named queue. Jobs in a queue start in submission order, at
// most Concurrency at a time.
type Queue struct {
	Name        string `json:"name"`
	Concurrency int    `json:"concurrency"`
	Running     int    `json:"running" doc:"Jobs of this queue running now."`
	Queued      int    `json:"queued" doc:"Jobs waiting in this queue."`
}

// QueueList is the response to GET /v2/queues.
type QueueList struct {
	Queues []Queue `json:"queues"`
}

// PutQueueRequest is the body of PUT /v2/queues/{name}.
type PutQueueRequest struct {
	Concurrency int `json:"concurrency" minimum:"1" maximum:"1000"`
}

// TemplateParam declares a template parameter.
type TemplateParam struct {
	Type        string `json:"type,omitempty" enum:"string,int,bool" doc:"Defaults to string."`
	Description string `json:"description,omitempty"`
	// Default makes the parameter optional.
	Default *string `json:"default,omitempty" doc:"Value used when the parameter is not given. Without a default the parameter is required."`
	// Pattern is a regular expression the whole value must match.
	Pattern string   `json:"pattern,omitempty" doc:"Regular expression the whole value must match."`
	Enum    []string `json:"enum,omitempty" doc:"Allowed values."`
}

// TemplateSpec is a saved session or job with parameters. Placeholders
// written {{name}} are replaced in argv elements, env values, cwd and job
// env; never in shell command lines, which receive parameters as
// SHHTTP_PARAM_<NAME> environment variables instead.
type TemplateSpec struct {
	Description string                   `json:"description,omitempty"`
	Params      map[string]TemplateParam `json:"params,omitempty"`
	Session     *SessionSpec             `json:"session,omitempty" doc:"Set exactly one of session and job."`
	Job         *JobSpec                 `json:"job,omitempty"`
}

// Template is a stored template.
type Template struct {
	Name string `json:"name"`
	TemplateSpec
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	// UpdatedBy is the key that last wrote the template.
	UpdatedBy string `json:"updated_by"`
}

// TemplateList is the response to GET /v2/templates.
type TemplateList struct {
	Templates []Template `json:"templates"`
}

// RunTemplateRequest is the body of POST /v2/templates/{name}/run.
type RunTemplateRequest struct {
	Params map[string]string `json:"params,omitempty" doc:"Parameter values, as strings."`
}

// RunTemplateResponse holds what the template started.
type RunTemplateResponse struct {
	Session *Session `json:"session,omitempty"`
	Job     *Job     `json:"job,omitempty"`
}
