package server

import (
	"strings"
	"testing"
	"time"

	"github.com/codemug/shhttp/pkg/api"
)

func allScopesKey(t *testing.T, ts *testServer, policy api.Policy) string {
	t.Helper()
	return ts.newKey(t, api.CreateKeyRequest{Scopes: []string{
		api.ScopeSessionsRun, api.ScopeSessionsRead, api.ScopeJobsRun, api.ScopeJobsRead,
		api.ScopeQueuesWrite, api.ScopeTemplatesRun, api.ScopeTemplatesRead, api.ScopeTemplatesWrite,
	}, Policy: policy})
}

func waitJob(t *testing.T, ts *testServer, key, id string) api.Job {
	t.Helper()
	var j api.Job
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		expect(t, ts.do(t, key, "GET", "/v2/jobs/"+id, nil, &j), 200)
		if j.State.Finished() {
			return j
		}
	}
	t.Fatalf("job %s did not finish: %+v", id, j)
	return j
}

func TestJobsOverHTTP(t *testing.T) {
	ts := newServer(t)
	key := allScopesKey(t, ts, api.Policy{})
	spec := api.JobSpec{Name: "build", Steps: []api.JobStep{
		{Name: "one", Spec: api.SessionSpec{Shell: "echo one"}},
		{Name: "two", Spec: api.SessionSpec{Argv: []string{"echo", "two"}}},
	}}
	var j api.Job
	expect(t, ts.do(t, key, "POST", "/v2/jobs", spec, &j), 201)
	if j.ID == "" || len(j.Steps) != 2 {
		t.Fatalf("submitted %+v", j)
	}

	// Follow the job's events until it finishes.
	evs := streamEvents(t, ts, key, "/v2/jobs/"+j.ID+"/events?follow=true", nil)
	var types []string
	for _, e := range evs {
		types = append(types, string(e.Type))
	}
	if strings.Join(types, ",") != "job_started,step_started,step_finished,step_started,step_finished,job_finished" {
		t.Fatalf("events %v", types)
	}
	if evs[len(evs)-1].State != api.SessionState(api.JobSucceeded) {
		t.Fatalf("last event %+v", evs[len(evs)-1])
	}

	j = waitJob(t, ts, key, j.ID)
	var res api.Session
	expect(t, ts.do(t, key, "GET", "/v2/sessions/"+j.Steps[1].SessionID, nil, &res), 200)
	out := streamEvents(t, ts, key, "/v2/sessions/"+j.Steps[1].SessionID+"/events", nil)
	if string(out[1].Data) != "two\n" {
		t.Fatalf("step output %+v", out)
	}

	var list api.JobList
	expect(t, ts.do(t, key, "GET", "/v2/jobs?state=succeeded", nil, &list), 200)
	if len(list.Jobs) != 1 || list.Jobs[0].ID != j.ID {
		t.Fatalf("list %+v", list)
	}
	expect(t, ts.do(t, key, "DELETE", "/v2/jobs/"+j.ID, nil, nil), 204)
	expect(t, ts.do(t, key, "GET", "/v2/sessions/"+j.Steps[0].SessionID, nil, nil), 404)
}

func TestJobAccess(t *testing.T) {
	ts := newServer(t)
	owner := allScopesKey(t, ts, api.Policy{})
	other := allScopesKey(t, ts, api.Policy{})
	sessionsOnly := ts.newKey(t, api.CreateKeyRequest{})
	no := false
	noShell := allScopesKey(t, ts, api.Policy{AllowShell: &no})

	spec := api.JobSpec{Steps: []api.JobStep{{Spec: api.SessionSpec{Shell: "sleep 30"}}}}
	expect(t, ts.do(t, sessionsOnly, "POST", "/v2/jobs", spec, nil), 403)
	// Every step is checked against the policy at submission.
	expect(t, ts.do(t, noShell, "POST", "/v2/jobs", spec, nil), 403)
	// Schema and graph errors.
	expect(t, ts.do(t, owner, "POST", "/v2/jobs", api.JobSpec{}, nil), 400)
	bad := api.JobSpec{Steps: []api.JobStep{{Name: "a", DependsOn: []string{"b"}, Spec: api.SessionSpec{Argv: []string{"true"}}}}}
	expect(t, ts.do(t, owner, "POST", "/v2/jobs", bad, nil), 400)

	var j api.Job
	expect(t, ts.do(t, owner, "POST", "/v2/jobs", spec, &j), 201)
	expect(t, ts.do(t, other, "GET", "/v2/jobs/"+j.ID, nil, nil), 404)
	expect(t, ts.do(t, other, "POST", "/v2/jobs/"+j.ID+"/cancel", nil, nil), 404)
	expect(t, ts.do(t, owner, "POST", "/v2/jobs/"+j.ID+"/cancel", nil, &j), 200)
	if j.State != api.JobCancelled {
		t.Fatalf("cancelled job state %s", j.State)
	}
	expect(t, ts.do(t, owner, "POST", "/v2/jobs/"+j.ID+"/cancel", nil, nil), 409)
}

func TestQueuesOverHTTP(t *testing.T) {
	ts := newServer(t)
	key := allScopesKey(t, ts, api.Policy{})
	reader := ts.newKey(t, api.CreateKeyRequest{Scopes: []string{api.ScopeJobsRead}})

	var ql api.QueueList
	expect(t, ts.do(t, reader, "GET", "/v2/queues", nil, &ql), 200)
	if len(ql.Queues) != 1 || ql.Queues[0].Name != "default" || ql.Queues[0].Concurrency != 1 {
		t.Fatalf("queues %+v", ql)
	}
	expect(t, ts.do(t, reader, "PUT", "/v2/queues/deploys", api.PutQueueRequest{Concurrency: 2}, nil), 403)
	var q api.Queue
	expect(t, ts.do(t, key, "PUT", "/v2/queues/deploys", api.PutQueueRequest{Concurrency: 2}, &q), 200)
	if q.Name != "deploys" || q.Concurrency != 2 {
		t.Fatalf("queue %+v", q)
	}
	expect(t, ts.do(t, key, "PUT", "/v2/queues/deploys", api.PutQueueRequest{Concurrency: 0}, nil), 422)
	expect(t, ts.do(t, key, "POST", "/v2/jobs", api.JobSpec{Queue: "missing", Steps: []api.JobStep{{Spec: api.SessionSpec{Argv: []string{"true"}}}}}, nil), 400)
	expect(t, ts.do(t, key, "DELETE", "/v2/queues/default", nil, nil), 400)
	expect(t, ts.do(t, key, "DELETE", "/v2/queues/deploys", nil, nil), 204)
	expect(t, ts.do(t, key, "DELETE", "/v2/queues/deploys", nil, nil), 404)
}

func TestTemplatesOverHTTP(t *testing.T) {
	ts := newServer(t)
	admin := allScopesKey(t, ts, api.Policy{})
	runner := ts.newKey(t, api.CreateKeyRequest{Scopes: []string{api.ScopeTemplatesRun, api.ScopeSessionsRead}})
	limited := ts.newKey(t, api.CreateKeyRequest{Scopes: []string{api.ScopeTemplatesRun}, Policy: api.Policy{Templates: []string{"other"}}})
	def := "world"
	greet := api.TemplateSpec{
		Description: "say hello",
		Params:      map[string]api.TemplateParam{"name": {Pattern: "[a-z]+", Default: &def}},
		Session:     &api.SessionSpec{Argv: []string{"echo", "hello {{name}}"}},
	}
	expect(t, ts.do(t, runner, "PUT", "/v2/templates/greet", greet, nil), 403)
	var tpl api.Template
	expect(t, ts.do(t, admin, "PUT", "/v2/templates/greet", greet, &tpl), 200)
	if tpl.Name != "greet" || tpl.UpdatedBy == "" {
		t.Fatalf("template %+v", tpl)
	}
	unsafe := api.TemplateSpec{Params: greet.Params, Session: &api.SessionSpec{Shell: "echo {{name}}"}}
	expect(t, ts.do(t, admin, "PUT", "/v2/templates/unsafe", unsafe, nil), 400)

	var list api.TemplateList
	expect(t, ts.do(t, runner, "GET", "/v2/templates", nil, &list), 200)
	if len(list.Templates) != 1 {
		t.Fatalf("templates %+v", list)
	}

	// A key with only templates:run starts sessions through templates.
	var res api.RunTemplateResponse
	expect(t, ts.do(t, runner, "POST", "/v2/templates/greet/run", api.RunTemplateRequest{Params: map[string]string{"name": "ada"}}, &res), 201)
	if res.Session == nil || res.Session.Spec.Argv[1] != "hello ada" {
		t.Fatalf("run %+v", res)
	}
	out := streamEvents(t, ts, runner, "/v2/sessions/"+res.Session.ID+"/events?follow=true", nil)
	if string(out[1].Data) != "hello ada\n" {
		t.Fatalf("output %+v", out)
	}
	expect(t, ts.do(t, runner, "POST", "/v2/templates/greet/run", nil, &res), 201)
	expect(t, ts.do(t, runner, "POST", "/v2/templates/greet/run", api.RunTemplateRequest{Params: map[string]string{"name": "A!"}}, nil), 400)
	expect(t, ts.do(t, runner, "POST", "/v2/templates/missing/run", nil, nil), 404)
	expect(t, ts.do(t, limited, "POST", "/v2/templates/greet/run", nil, nil), 403)
	// templates:run alone does not allow direct sessions.
	expect(t, ts.do(t, runner, "POST", "/v2/sessions", api.SessionSpec{Argv: []string{"true"}}, nil), 403)

	// Job templates submit jobs.
	pipeline := api.TemplateSpec{
		Params: map[string]api.TemplateParam{"msg": {}},
		Job:    &api.JobSpec{Steps: []api.JobStep{{Spec: api.SessionSpec{Shell: `echo "$SHHTTP_PARAM_MSG"`}}}},
	}
	expect(t, ts.do(t, admin, "PUT", "/v2/templates/pipeline", pipeline, nil), 200)
	expect(t, ts.do(t, admin, "POST", "/v2/templates/pipeline/run", nil, nil), 400) // msg is required
	expect(t, ts.do(t, admin, "POST", "/v2/templates/pipeline/run", api.RunTemplateRequest{Params: map[string]string{"msg": "$(id)"}}, &res), 201)
	j := waitJob(t, ts, admin, res.Job.ID)
	out = streamEvents(t, ts, admin, "/v2/sessions/"+j.Steps[0].SessionID+"/events", nil)
	if string(out[1].Data) != "$(id)\n" {
		t.Fatalf("parameter was interpreted by the shell: %q", out[1].Data)
	}

	expect(t, ts.do(t, admin, "DELETE", "/v2/templates/greet", nil, nil), 204)
	expect(t, ts.do(t, admin, "GET", "/v2/templates/greet", nil, nil), 404)
}
