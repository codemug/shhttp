package template

import (
	"errors"
	"strings"
	"testing"

	"github.com/codemug/shhttp/pkg/api"
)

func ptr(s string) *string { return &s }

func TestValidate(t *testing.T) {
	branch := map[string]api.TemplateParam{"branch": {Pattern: `[a-z]+`, Default: ptr("main")}}
	good := []api.TemplateSpec{
		{Params: branch, Session: &api.SessionSpec{Argv: []string{"git", "checkout", "{{branch}}"}}},
		{Params: branch, Session: &api.SessionSpec{Shell: `git checkout "$SHHTTP_PARAM_BRANCH"`}},
		{Params: branch, Job: &api.JobSpec{Env: map[string]string{"B": "{{ branch }}"}, Steps: []api.JobStep{{Spec: api.SessionSpec{Argv: []string{"--ref={{branch}}"}}}}}},
	}
	for _, spec := range good {
		if err := Validate(spec); err != nil {
			t.Errorf("Validate(%+v): %v", spec, err)
		}
	}
	bad := []api.TemplateSpec{
		{},
		{Session: &api.SessionSpec{Argv: []string{"x"}}, Job: &api.JobSpec{}},
		{Session: &api.SessionSpec{Argv: []string{"{{missing}}"}}},
		{Params: branch, Session: &api.SessionSpec{Shell: "git checkout {{branch}}"}},
		{Params: map[string]api.TemplateParam{"1x": {}}, Session: &api.SessionSpec{Argv: []string{"x"}}},
		{Params: map[string]api.TemplateParam{"n": {Type: "float"}}, Session: &api.SessionSpec{Argv: []string{"x"}}},
		{Params: map[string]api.TemplateParam{"n": {Pattern: "("}}, Session: &api.SessionSpec{Argv: []string{"x"}}},
		{Params: map[string]api.TemplateParam{"n": {Type: "int", Default: ptr("ten")}}, Session: &api.SessionSpec{Argv: []string{"x"}}},
		{Params: branch, Job: &api.JobSpec{Steps: []api.JobStep{{Spec: api.SessionSpec{Shell: "echo {{branch}}"}}}}},
	}
	for _, spec := range bad {
		var inv *InvalidError
		if err := Validate(spec); !errors.As(err, &inv) {
			t.Errorf("Validate(%+v) = %v, want InvalidError", spec, err)
		}
	}
}

func TestRenderSession(t *testing.T) {
	tpl := api.Template{Name: "deploy", TemplateSpec: api.TemplateSpec{
		Params: map[string]api.TemplateParam{
			"branch":  {Pattern: `[a-z0-9/._-]+`, Default: ptr("main")},
			"retries": {Type: "int"},
			"mode":    {Enum: []string{"fast", "safe"}, Default: ptr("safe")},
		},
		Session: &api.SessionSpec{
			Argv: []string{"./deploy", "--branch={{branch}}", "--retries", "{{retries}}"},
			Env:  map[string]string{"MODE": "{{mode}}"},
			Cwd:  "/srv/{{branch}}",
		},
	}}
	s, j, err := Render(tpl, map[string]string{"retries": "3", "branch": "release/2"})
	if err != nil || j != nil {
		t.Fatalf("Render: %v", err)
	}
	if got := strings.Join(s.Argv, " "); got != "./deploy --branch=release/2 --retries 3" {
		t.Fatalf("argv %q", got)
	}
	if s.Env["MODE"] != "safe" || s.Env["SHHTTP_PARAM_BRANCH"] != "release/2" || s.Cwd != "/srv/release/2" {
		t.Fatalf("env %v cwd %q", s.Env, s.Cwd)
	}
	if s.Labels["shhttp.template"] != "deploy" {
		t.Fatalf("labels %v", s.Labels)
	}
	// The stored template is not modified.
	if tpl.Session.Argv[1] != "--branch={{branch}}" {
		t.Fatal("Render changed the template")
	}

	for _, params := range []map[string]string{
		{},                                   // retries missing
		{"retries": "x"},                     // not an int
		{"retries": "1", "branch": "A B"},    // pattern
		{"retries": "1", "mode": "reckless"}, // enum
		{"retries": "1", "extra": "1"},       // unknown
	} {
		var inv *InvalidError
		if _, _, err := Render(tpl, params); !errors.As(err, &inv) {
			t.Errorf("Render(%v) = %v, want InvalidError", params, err)
		}
	}
}

func TestRenderJob(t *testing.T) {
	tpl := api.Template{Name: "ci", TemplateSpec: api.TemplateSpec{
		Params: map[string]api.TemplateParam{"ref": {}},
		Job: &api.JobSpec{
			Env:   map[string]string{"REF": "{{ref}}"},
			Steps: []api.JobStep{{Name: "a", Spec: api.SessionSpec{Shell: `echo "$SHHTTP_PARAM_REF"`}}},
		},
	}}
	_, j, err := Render(tpl, map[string]string{"ref": "v1; rm -rf /"})
	if err != nil {
		t.Fatal(err)
	}
	st := j.Steps[0].Spec
	if j.Env["REF"] != "v1; rm -rf /" || st.Shell != `echo "$SHHTTP_PARAM_REF"` || st.Env["SHHTTP_PARAM_REF"] != "v1; rm -rf /" {
		t.Fatalf("job %+v", j)
	}
}
