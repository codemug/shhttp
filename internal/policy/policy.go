// Package policy validates API key policies and checks session requests
// against them.
package policy

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/codemug/shhttp/v2/pkg/api"
)

// DeniedError reports that a policy forbids a request.
type DeniedError struct{ Reason string }

func (e *DeniedError) Error() string { return "denied by key policy: " + e.Reason }

func denied(format string, args ...any) error {
	return &DeniedError{Reason: fmt.Sprintf(format, args...)}
}

// Validate checks that a policy is well formed.
func Validate(p api.Policy) error {
	for _, pat := range p.Commands {
		if _, err := compile(pat); err != nil {
			return fmt.Errorf("policy.commands: %w", err)
		}
	}
	for _, pat := range p.EnvAllow {
		if _, err := compile(pat); err != nil {
			return fmt.Errorf("policy.env_allow: %w", err)
		}
	}
	for _, root := range p.CwdRoots {
		if !filepath.IsAbs(root) {
			return fmt.Errorf("policy.cwd_roots: %q is not an absolute path", root)
		}
	}
	if p.MaxConcurrentSessions < 0 {
		return fmt.Errorf("policy.max_concurrent_sessions must not be negative")
	}
	if p.MaxOutputBytes < 0 {
		return fmt.Errorf("policy.max_output_bytes must not be negative")
	}
	return nil
}

// compile anchors pat so that it must match the whole string.
func compile(pat string) (*regexp.Regexp, error) {
	return regexp.Compile(`^(?:` + pat + `)$`)
}

func matchAny(patterns []string, s string) bool {
	for _, pat := range patterns {
		if re, err := compile(pat); err == nil && re.MatchString(s) {
			return true
		}
	}
	return false
}

// Request is what a session is about to do, after argv[0] has been resolved
// to an absolute path and the working directory has been defaulted.
type Request struct {
	Shell   bool
	TTY     bool
	Path    string // absolute program path; for shell sessions, the shell
	Cwd     string // absolute; empty means the server's working directory
	EnvKeys []string
	Timeout time.Duration
}

// DefaultCwd returns the working directory to use when a session sets none.
func DefaultCwd(p api.Policy) string {
	if len(p.CwdRoots) > 0 {
		return p.CwdRoots[0]
	}
	return ""
}

// Check returns the timeout to apply, or a *DeniedError. evalSymlinks
// resolves a path; it is a parameter so that tests can stub it.
func Check(p api.Policy, r Request, evalSymlinks func(string) (string, error)) (time.Duration, error) {
	if r.Shell && !p.ShellAllowed() {
		return 0, denied("shell sessions are not allowed")
	}
	if r.TTY && !p.TTYAllowed() {
		return 0, denied("TTY sessions are not allowed")
	}
	if !r.Shell && len(p.Commands) > 0 && !matchAny(p.Commands, r.Path) {
		return 0, denied("command %q is not in the allowed list", r.Path)
	}
	if len(p.CwdRoots) > 0 {
		if r.Cwd == "" {
			return 0, denied("a working directory inside the allowed roots is required")
		}
		cwd, err := evalSymlinks(r.Cwd)
		if err != nil {
			return 0, denied("working directory %q: %v", r.Cwd, err)
		}
		ok := false
		for _, root := range p.CwdRoots {
			root, err := evalSymlinks(root)
			if err != nil {
				continue
			}
			if rel, err := filepath.Rel(root, cwd); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				ok = true
				break
			}
		}
		if !ok {
			return 0, denied("working directory %q is outside the allowed roots", r.Cwd)
		}
	}
	if len(p.EnvAllow) > 0 {
		for _, k := range r.EnvKeys {
			if !matchAny(p.EnvAllow, k) {
				return 0, denied("environment variable %q is not allowed", k)
			}
		}
	}
	timeout := r.Timeout
	if max := p.MaxTimeout.Std(); max > 0 {
		if timeout == 0 {
			timeout = max
		} else if timeout > max {
			return 0, denied("timeout %s exceeds the maximum of %s", timeout, max)
		}
	}
	return timeout, nil
}

// Notes describes consequences of a policy that are easy to miss.
func Notes(p api.Policy) []string {
	var notes []string
	if len(p.Commands) > 0 && p.ShellAllowed() {
		notes = append(notes, "this key may run shell sessions, which can start any program, so the commands list does not restrict it; set policy.allow_shell to false to enforce it")
	}
	return notes
}
