package policy

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/codemug/shhttp/pkg/api"
)

func ptr[T any](v T) *T { return &v }

func TestCheck(t *testing.T) {
	root := t.TempDir()
	inside := filepath.Join(root, "work")
	if err := os.Mkdir(inside, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	escape := filepath.Join(root, "escape")
	if err := os.Symlink(outside, escape); err != nil {
		t.Fatal(err)
	}

	p := api.Policy{
		AllowShell: ptr(false),
		Commands:   []string{"/usr/bin/(git|make)"},
		CwdRoots:   []string{root},
		EnvAllow:   []string{"CI_.*"},
		MaxTimeout: api.Duration(time.Minute),
	}
	if err := Validate(p); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name    string
		req     Request
		ok      bool
		timeout time.Duration
	}{
		{"allowed", Request{Path: "/usr/bin/git", Cwd: inside, EnvKeys: []string{"CI_X"}}, true, time.Minute},
		{"explicit timeout", Request{Path: "/usr/bin/make", Cwd: root, Timeout: time.Second}, true, time.Second},
		{"shell", Request{Shell: true, Path: "/bin/sh", Cwd: root}, false, 0},
		{"command not listed", Request{Path: "/usr/bin/gitx", Cwd: root}, false, 0},
		{"partial match is not enough", Request{Path: "/opt/usr/bin/git", Cwd: root}, false, 0},
		{"cwd outside", Request{Path: "/usr/bin/git", Cwd: outside}, false, 0},
		{"cwd escapes through symlink", Request{Path: "/usr/bin/git", Cwd: escape}, false, 0},
		{"cwd parent", Request{Path: "/usr/bin/git", Cwd: filepath.Dir(root)}, false, 0},
		{"env not allowed", Request{Path: "/usr/bin/git", Cwd: root, EnvKeys: []string{"LD_PRELOAD"}}, false, 0},
		{"timeout too long", Request{Path: "/usr/bin/git", Cwd: root, Timeout: time.Hour}, false, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			timeout, err := Check(p, c.req, filepath.EvalSymlinks)
			var d *DeniedError
			if c.ok && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !c.ok && !errors.As(err, &d) {
				t.Fatalf("expected denial, got %v", err)
			}
			if timeout != c.timeout {
				t.Fatalf("timeout = %s, want %s", timeout, c.timeout)
			}
		})
	}
}

func TestEmptyPolicyAllowsEverything(t *testing.T) {
	if _, err := Check(api.Policy{}, Request{Shell: true, Path: "/bin/sh", EnvKeys: []string{"X"}}, filepath.EvalSymlinks); err != nil {
		t.Fatal(err)
	}
}

func TestValidate(t *testing.T) {
	for _, p := range []api.Policy{
		{Commands: []string{"("}},
		{EnvAllow: []string{"["}},
		{CwdRoots: []string{"relative"}},
		{MaxConcurrentSessions: -1},
	} {
		if Validate(p) == nil {
			t.Errorf("Validate(%+v) = nil", p)
		}
	}
}

func TestNotes(t *testing.T) {
	if len(Notes(api.Policy{Commands: []string{"x"}})) != 1 {
		t.Fatal("expected a note about shell access")
	}
	if len(Notes(api.Policy{Commands: []string{"x"}, AllowShell: ptr(false)})) != 0 {
		t.Fatal("unexpected note")
	}
}
