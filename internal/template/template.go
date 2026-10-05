// Package template validates and renders templates: saved session or job
// specs with typed parameters.
//
// Placeholders written {{name}} are replaced in argv elements, env values
// and cwd (of a session, or of every job step, plus the job's env). They are
// never replaced inside shell command lines, where a value could inject
// shell syntax; every rendered session instead gets each parameter as the
// environment variable SHHTTP_PARAM_<NAME>.
package template

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/codemug/shhttp/v2/internal/store"
	"github.com/codemug/shhttp/v2/pkg/api"
)

// ErrNotFound means the template does not exist.
var ErrNotFound = errors.New("template not found")

// InvalidError reports a malformed template or parameter value.
type InvalidError struct{ Reason string }

func (e *InvalidError) Error() string { return e.Reason }

func invalid(format string, args ...any) error {
	return &InvalidError{Reason: fmt.Sprintf(format, args...)}
}

var (
	nameRe      = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)
	paramRe     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)
	placeholder = regexp.MustCompile(`\{\{\s*([^{}\s]*)\s*\}\}`)
)

// ValidName reports whether name can name a template.
func ValidName(name string) bool { return nameRe.MatchString(name) }

// Validate checks a template spec.
func Validate(spec api.TemplateSpec) error {
	if (spec.Session == nil) == (spec.Job == nil) {
		return invalid("set exactly one of session and job")
	}
	for name, p := range spec.Params {
		if !paramRe.MatchString(name) {
			return invalid("parameter name %q must start with a letter or '_' and contain only letters, digits and '_'", name)
		}
		switch p.Type {
		case "", "string", "int", "bool":
		default:
			return invalid("parameter %q: type must be string, int or bool", name)
		}
		if p.Pattern != "" {
			if _, err := regexp.Compile(`^(?:` + p.Pattern + `)$`); err != nil {
				return invalid("parameter %q: pattern: %v", name, err)
			}
		}
		if p.Default != nil {
			if err := checkValue(name, p, *p.Default); err != nil {
				return invalid("parameter %q: default: %v", name, err)
			}
		}
	}
	var err error
	check := func(where, s string) {
		for _, m := range placeholder.FindAllStringSubmatch(s, -1) {
			if _, ok := spec.Params[m[1]]; !ok && err == nil {
				err = invalid("%s uses {{%s}}, which is not a declared parameter", where, m[1])
			}
		}
	}
	checkSession := func(where string, s api.SessionSpec) {
		if strings.Contains(s.Shell, "{{") && err == nil {
			err = invalid("%s: placeholders are not substituted in shell command lines; use \"$SHHTTP_PARAM_<NAME>\" instead", where)
		}
		for i, a := range s.Argv {
			check(fmt.Sprintf("%s argv[%d]", where, i), a)
		}
		for k, v := range s.Env {
			check(fmt.Sprintf("%s env %s", where, k), v)
		}
		check(where+" cwd", s.Cwd)
	}
	if spec.Session != nil {
		checkSession("session", *spec.Session)
	}
	if spec.Job != nil {
		for k, v := range spec.Job.Env {
			check("job env "+k, v)
		}
		for i, st := range spec.Job.Steps {
			checkSession(fmt.Sprintf("step %d", i+1), st.Spec)
		}
	}
	return err
}

func checkValue(name string, p api.TemplateParam, v string) error {
	switch p.Type {
	case "int":
		if _, err := strconv.ParseInt(v, 10, 64); err != nil {
			return fmt.Errorf("%q is not an integer", v)
		}
	case "bool":
		if _, err := strconv.ParseBool(v); err != nil {
			return fmt.Errorf("%q is not true or false", v)
		}
	}
	if p.Pattern != "" && !regexp.MustCompile(`^(?:`+p.Pattern+`)$`).MatchString(v) {
		return fmt.Errorf("%q does not match %s", v, p.Pattern)
	}
	if len(p.Enum) > 0 && !slices.Contains(p.Enum, v) {
		return fmt.Errorf("%q is not one of %s", v, strings.Join(p.Enum, ", "))
	}
	return nil
}

// Render fills in a template's parameters. The result is a session spec or
// a job spec, depending on the template.
func Render(t api.Template, given map[string]string) (*api.SessionSpec, *api.JobSpec, error) {
	values := map[string]string{}
	for name, v := range given {
		p, ok := t.Params[name]
		if !ok {
			return nil, nil, invalid("template %s has no parameter %q", t.Name, name)
		}
		if err := checkValue(name, p, v); err != nil {
			return nil, nil, invalid("parameter %s: %v", name, err)
		}
		values[name] = v
	}
	for name, p := range t.Params {
		if _, ok := values[name]; ok {
			continue
		}
		if p.Default == nil {
			return nil, nil, invalid("parameter %s is required", name)
		}
		values[name] = *p.Default
	}
	sub := func(s string) string {
		return placeholder.ReplaceAllStringFunc(s, func(m string) string {
			return values[placeholder.FindStringSubmatch(m)[1]]
		})
	}
	paramEnv := func(env map[string]string) map[string]string {
		if len(values) == 0 {
			return env
		}
		out := map[string]string{}
		for k, v := range values {
			out["SHHTTP_PARAM_"+strings.ToUpper(k)] = v
		}
		maps.Copy(out, env)
		return out
	}
	renderSession := func(s api.SessionSpec) api.SessionSpec {
		s.Argv = slices.Clone(s.Argv)
		for i := range s.Argv {
			s.Argv[i] = sub(s.Argv[i])
		}
		env := map[string]string{}
		for k, v := range s.Env {
			env[k] = sub(v)
		}
		s.Env = paramEnv(env)
		if len(s.Env) == 0 {
			s.Env = nil
		}
		s.Cwd = sub(s.Cwd)
		s.Labels = maps.Clone(s.Labels)
		if s.Labels == nil {
			s.Labels = map[string]string{}
		}
		s.Labels["shhttp.template"] = t.Name
		return s
	}
	if t.Session != nil {
		s := renderSession(*t.Session)
		return &s, nil, nil
	}
	j := *t.Job
	j.Env = maps.Clone(j.Env)
	for k, v := range j.Env {
		j.Env[k] = sub(v)
	}
	j.Steps = slices.Clone(j.Steps)
	for i := range j.Steps {
		j.Steps[i].Spec = renderSession(j.Steps[i].Spec)
	}
	j.Labels = maps.Clone(j.Labels)
	if j.Labels == nil {
		j.Labels = map[string]string{}
	}
	j.Labels["shhttp.template"] = t.Name
	return nil, &j, nil
}

// Service stores templates.
type Service struct {
	store *store.Store
}

// NewService returns a Service using st.
func NewService(st *store.Store) *Service { return &Service{store: st} }

// Put validates and stores a template under name, replacing any existing
// one.
func (s *Service) Put(ctx context.Context, name string, spec api.TemplateSpec, keyID string) (api.Template, error) {
	if !ValidName(name) {
		return api.Template{}, invalid("template names are 1-64 letters, digits, '_', '.' or '-'")
	}
	if err := Validate(spec); err != nil {
		return api.Template{}, err
	}
	return s.store.PutTemplate(ctx, name, spec, keyID, time.Now().UTC())
}

// Get returns a template.
func (s *Service) Get(ctx context.Context, name string) (api.Template, error) {
	t, err := s.store.GetTemplate(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		return t, ErrNotFound
	}
	return t, err
}

// List returns every template.
func (s *Service) List(ctx context.Context) ([]api.Template, error) {
	return s.store.ListTemplates(ctx)
}

// Delete removes a template.
func (s *Service) Delete(ctx context.Context, name string) error {
	err := s.store.DeleteTemplate(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		return ErrNotFound
	}
	return err
}
