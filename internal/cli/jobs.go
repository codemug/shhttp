package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/codemug/shhttp/pkg/api"
	"github.com/codemug/shhttp/pkg/client"
)

const jobUsage = `Usage: shhttp job <command>

  submit [-f FILE] [-w]        submit a job described in JSON (default: stdin); -w waits for it
  ls [-state S] [-queue Q] [-json]
  get <id>                     show a job as JSON
  watch <id>                   print the job's progress until it ends; exit 0 if it succeeded
  logs [-step NAME] <id>       print the output of the job's steps
  cancel <id>
  rm <id>                      delete the job and its step sessions
`

const queueUsage = `Usage: shhttp queue <command>

  ls
  set <name> <concurrency>     create or resize a queue
  rm <name>
`

const templateUsage = `Usage: shhttp template <command>

  ls
  get <name>
  put <name> [-f FILE]         create or replace a template from JSON (default: stdin)
  rm <name>
  run [-p NAME=VALUE]... [-d] <name>
                               run it: attach to its session or watch its job; -d only starts it
`

// subcommand dispatches "shhttp <group> <command>".
func (a *app) subcommand(usageText string, args []string, cmds map[string]func([]string) error) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "-help" || args[0] == "--help" {
		fmt.Fprint(a.env.Stderr, usageText)
		if len(args) == 0 {
			return errUsage
		}
		return nil
	}
	if run, ok := cmds[args[0]]; ok {
		return run(args[1:])
	}
	fmt.Fprintf(a.env.Stderr, "shhttp: unknown command %q\n\n%s", args[0], usageText)
	return errUsage
}

// readJSON decodes JSON from a file, or stdin for "-".
func (a *app) readJSON(path string, v any) error {
	var r io.Reader = a.env.Stdin
	if path != "-" {
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		r = f
	}
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("reading %s: %w", path, err)
	}
	return nil
}

func (a *app) jobCmd(args []string) error {
	return a.subcommand(jobUsage, args, map[string]func([]string) error{
		"submit": a.jobSubmit,
		"ls":     a.jobList,
		"get":    a.jobGet,
		"watch":  a.jobWatchCmd,
		"logs":   a.jobLogs,
		"cancel": a.jobCancel,
		"rm":     a.jobRemove,
	})
}

func (a *app) jobSubmit(args []string) error {
	fs := a.flags("job submit", "[-f FILE] [-w]")
	file := fs.String("f", "-", "JSON job spec, or - for stdin")
	wait := fs.Bool("w", false, "wait for the job and print its progress")
	if err := parse(fs, args, 0, 0); err != nil {
		return err
	}
	var spec api.JobSpec
	if err := a.readJSON(*file, &spec); err != nil {
		return err
	}
	j, err := a.client().SubmitJob(a.ctx, spec)
	if err != nil {
		return err
	}
	if !*wait {
		fmt.Fprintln(a.env.Stdout, j.ID)
		return nil
	}
	return a.watchJob(j.ID)
}

func (a *app) jobList(args []string) error {
	fs := a.flags("job ls", "[flags]")
	state := fs.String("state", "", "only jobs in this state")
	queue := fs.String("queue", "", "only jobs of this queue")
	limit := fs.Int("limit", 50, "most jobs to show")
	asJSON := fs.Bool("json", false, "print JSON")
	if err := parse(fs, args, 0, 0); err != nil {
		return err
	}
	list, err := a.client().ListJobs(a.ctx, &client.JobListOptions{State: api.JobState(*state), Queue: *queue, Limit: *limit})
	if err != nil {
		return err
	}
	if *asJSON {
		return a.printJSON(list.Jobs)
	}
	w := tabwriter.NewWriter(a.env.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tNAME\tSTATE\tQUEUE\tSTEPS\tCREATED")
	for _, j := range list.Jobs {
		done := 0
		for _, s := range j.Steps {
			if s.State != api.StepPending && s.State != api.StepRunning {
				done++
			}
		}
		queue := j.Spec.Queue
		if queue == "" {
			queue = "-"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d/%d\t%s\n", j.ID, j.Spec.Name, j.State, queue, done, len(j.Steps), ago(j.CreatedAt))
	}
	return w.Flush()
}

func (a *app) jobGet(args []string) error {
	fs := a.flags("job get", "<id>")
	if err := parse(fs, args, 1, 1); err != nil {
		return err
	}
	j, err := a.client().GetJob(a.ctx, fs.Arg(0))
	if err != nil {
		return err
	}
	return a.printJSON(j)
}

func (a *app) jobWatchCmd(args []string) error {
	fs := a.flags("job watch", "<id>")
	if err := parse(fs, args, 1, 1); err != nil {
		return err
	}
	return a.watchJob(fs.Arg(0))
}

// watchJob prints a job's events until it ends and fails unless it
// succeeded.
func (a *app) watchJob(jobID string) error {
	var final api.JobState
	for e, err := range a.client().JobEvents(a.ctx, jobID, &client.EventsOptions{Follow: true}) {
		if err != nil {
			return err
		}
		switch e.Type {
		case api.EventJobStarted:
			fmt.Fprintf(a.env.Stdout, "%s started\n", jobID)
		case api.EventStepStarted:
			fmt.Fprintf(a.env.Stdout, "  %-20s started  %s\n", e.Step, e.SessionID)
		case api.EventStepFinished:
			detail := ""
			switch {
			case e.ExitCode != nil:
				detail = fmt.Sprintf(" (exit %d)", *e.ExitCode)
			case e.Signal != "":
				detail = " (" + e.Signal + ")"
			}
			if e.Error != "" && e.ExitCode == nil {
				detail += ": " + e.Error
			}
			fmt.Fprintf(a.env.Stdout, "  %-20s %s%s\n", e.Step, e.State, detail)
		case api.EventJobFinished:
			final = api.JobState(e.State)
			fmt.Fprintf(a.env.Stdout, "%s %s\n", jobID, e.State)
		}
	}
	if final != api.JobSucceeded {
		return exitError{1}
	}
	return nil
}

func (a *app) jobLogs(args []string) error {
	fs := a.flags("job logs", "[-step NAME] <id>")
	step := fs.String("step", "", "only this step")
	if err := parse(fs, args, 1, 1); err != nil {
		return err
	}
	c := a.client()
	j, err := c.GetJob(a.ctx, fs.Arg(0))
	if err != nil {
		return err
	}
	for _, s := range j.Steps {
		if s.SessionID == "" || (*step != "" && s.Name != *step) {
			continue
		}
		if *step == "" {
			fmt.Fprintf(a.env.Stdout, "==> %s (%s)\n", s.Name, s.State)
		}
		for e, err := range c.Events(a.ctx, s.SessionID, nil) {
			if err != nil {
				return err
			}
			switch e.Type {
			case api.EventStdout:
				a.env.Stdout.Write(e.Data)
			case api.EventStderr:
				a.env.Stderr.Write(e.Data)
			}
		}
	}
	return nil
}

func (a *app) jobCancel(args []string) error {
	fs := a.flags("job cancel", "<id>")
	if err := parse(fs, args, 1, 1); err != nil {
		return err
	}
	j, err := a.client().CancelJob(a.ctx, fs.Arg(0))
	if err != nil {
		return err
	}
	fmt.Fprintf(a.env.Stdout, "%s %s\n", j.ID, j.State)
	return nil
}

func (a *app) jobRemove(args []string) error {
	fs := a.flags("job rm", "<id>")
	if err := parse(fs, args, 1, 1); err != nil {
		return err
	}
	return a.client().DeleteJob(a.ctx, fs.Arg(0))
}

func (a *app) queueCmd(args []string) error {
	return a.subcommand(queueUsage, args, map[string]func([]string) error{
		"ls": func(args []string) error {
			if err := parse(a.flags("queue ls", ""), args, 0, 0); err != nil {
				return err
			}
			qs, err := a.client().Queues(a.ctx)
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(a.env.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tCONCURRENCY\tRUNNING\tQUEUED")
			for _, q := range qs {
				fmt.Fprintf(w, "%s\t%d\t%d\t%d\n", q.Name, q.Concurrency, q.Running, q.Queued)
			}
			return w.Flush()
		},
		"set": func(args []string) error {
			fs := a.flags("queue set", "<name> <concurrency>")
			if err := parse(fs, args, 2, 2); err != nil {
				return err
			}
			n, err := strconv.Atoi(fs.Arg(1))
			if err != nil {
				return fmt.Errorf("concurrency must be a number, not %q", fs.Arg(1))
			}
			_, err = a.client().PutQueue(a.ctx, fs.Arg(0), n)
			return err
		},
		"rm": func(args []string) error {
			fs := a.flags("queue rm", "<name>")
			if err := parse(fs, args, 1, 1); err != nil {
				return err
			}
			return a.client().DeleteQueue(a.ctx, fs.Arg(0))
		},
	})
}

func (a *app) templateCmd(args []string) error {
	return a.subcommand(templateUsage, args, map[string]func([]string) error{
		"ls": func(args []string) error {
			if err := parse(a.flags("template ls", ""), args, 0, 0); err != nil {
				return err
			}
			ts, err := a.client().ListTemplates(a.ctx)
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(a.env.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tKIND\tPARAMS\tDESCRIPTION")
			for _, t := range ts {
				kind := "session"
				if t.Job != nil {
					kind = "job"
				}
				var params []string
				for name := range t.Params {
					params = append(params, name)
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", t.Name, kind, strings.Join(slices.Sorted(slices.Values(params)), ","), t.Description)
			}
			return w.Flush()
		},
		"get": func(args []string) error {
			fs := a.flags("template get", "<name>")
			if err := parse(fs, args, 1, 1); err != nil {
				return err
			}
			t, err := a.client().GetTemplate(a.ctx, fs.Arg(0))
			if err != nil {
				return err
			}
			return a.printJSON(t)
		},
		"put": func(args []string) error {
			fs := a.flags("template put", "<name> [-f FILE]")
			file := fs.String("f", "-", "JSON template spec, or - for stdin")
			if err := parse(fs, args, 1, 1); err != nil {
				return err
			}
			var spec api.TemplateSpec
			if err := a.readJSON(*file, &spec); err != nil {
				return err
			}
			_, err := a.client().PutTemplate(a.ctx, fs.Arg(0), spec)
			return err
		},
		"rm": func(args []string) error {
			fs := a.flags("template rm", "<name>")
			if err := parse(fs, args, 1, 1); err != nil {
				return err
			}
			return a.client().DeleteTemplate(a.ctx, fs.Arg(0))
		},
		"run": a.templateRun,
	})
}

func (a *app) templateRun(args []string) error {
	fs := a.flags("template run", "[-p NAME=VALUE]... [-d] <name>")
	var params multiFlag
	fs.Var(&params, "p", "a parameter, NAME=VALUE (repeatable)")
	detach := fs.Bool("d", false, "only start it and print the session or job id")
	noStdin := fs.Bool("n", false, "do not send local stdin to a session template")
	if err := parse(fs, args, 1, 1); err != nil {
		return err
	}
	values, err := keyValues("p", params)
	if err != nil {
		return err
	}
	c := a.client()
	r, err := c.RunTemplate(a.ctx, fs.Arg(0), values)
	if err != nil {
		return err
	}
	switch {
	case r.Job != nil && *detach:
		fmt.Fprintln(a.env.Stdout, r.Job.ID)
		return nil
	case r.Job != nil:
		return a.watchJob(r.Job.ID)
	case *detach:
		fmt.Fprintln(a.env.Stdout, r.Session.ID)
		return nil
	}
	// Attach to the new session. Interactive attach needs sessions:run;
	// keys limited to templates:run watch it read-only instead.
	conn, err := c.Attach(a.ctx, r.Session.ID, nil)
	readonly := false
	if client.IsStatus(err, 403) {
		conn, err = c.Attach(a.ctx, r.Session.ID, &client.AttachOptions{ReadOnly: true})
		readonly = true
	}
	if err != nil {
		return err
	}
	defer conn.Close()
	return a.pump(conn, !readonly && !*noStdin, !readonly)
}
