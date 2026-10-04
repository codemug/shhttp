package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/codemug/shhttp/pkg/api"
)

const keyUsage = `Usage: shhttp key <command>

  create -name NAME -scope SCOPE [-scope SCOPE...] [-expires D] [-policy FILE] [-q]
  ls [-json]
  get <id>
  rotate [-grace D] [-q] <id>
  revoke [-kill-sessions] <id>

Key commands use SHHTTP_MASTER_KEY when it is set.
`

func (a *app) keyCmd(args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "-help" || args[0] == "--help" {
		fmt.Fprint(a.env.Stderr, keyUsage)
		if len(args) == 0 {
			return errUsage
		}
		return nil
	}
	switch args[0] {
	case "create":
		return a.keyCreate(args[1:])
	case "ls":
		return a.keyList(args[1:])
	case "get":
		return a.keyGet(args[1:])
	case "rotate":
		return a.keyRotate(args[1:])
	case "revoke":
		return a.keyRevoke(args[1:])
	}
	fmt.Fprintf(a.env.Stderr, "shhttp: unknown key command %q\n\n%s", args[0], keyUsage)
	return errUsage
}

func (a *app) keyCreate(args []string) error {
	fs := a.flags("key create", "-name NAME -scope SCOPE [flags]")
	name := fs.String("name", "", "a name for the key")
	var scopes multiFlag
	fs.Var(&scopes, "scope", "sessions:run, sessions:read or admin:read (repeatable or comma-separated)")
	expires := fs.Duration("expires", 0, "the key stops working after this long")
	policyFile := fs.String("policy", "", "JSON file with the key's policy, or - for stdin")
	quiet := fs.Bool("q", false, "print only the key")
	if err := parse(fs, args, 0, 0); err != nil {
		return err
	}
	req := api.CreateKeyRequest{Name: *name, ExpiresIn: api.Duration(*expires)}
	for _, s := range scopes {
		for _, part := range strings.Split(s, ",") {
			if part = strings.TrimSpace(part); part != "" {
				req.Scopes = append(req.Scopes, part)
			}
		}
	}
	if *policyFile != "" {
		var r io.Reader = a.env.Stdin
		if *policyFile != "-" {
			f, err := os.Open(*policyFile)
			if err != nil {
				return err
			}
			defer f.Close()
			r = f
		}
		dec := json.NewDecoder(r)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req.Policy); err != nil {
			return fmt.Errorf("reading policy: %w", err)
		}
	}
	k, err := a.masterClient().CreateKey(a.ctx, req)
	if err != nil {
		return err
	}
	if *quiet {
		fmt.Fprintln(a.env.Stdout, k.Secret)
		return nil
	}
	return a.printJSON(k)
}

func (a *app) keyList(args []string) error {
	fs := a.flags("key ls", "[-json]")
	asJSON := fs.Bool("json", false, "print JSON")
	if err := parse(fs, args, 0, 0); err != nil {
		return err
	}
	keys, err := a.masterClient().ListKeys(a.ctx)
	if err != nil {
		return err
	}
	if *asJSON {
		return a.printJSON(keys)
	}
	w := tabwriter.NewWriter(a.env.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tNAME\tSCOPES\tSTATUS\tLAST USED")
	for _, k := range keys {
		lastUsed := "never"
		if k.LastUsedAt != nil {
			lastUsed = ago(*k.LastUsedAt)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", k.ID, k.Name, strings.Join(k.Scopes, ","), keyStatus(k), lastUsed)
	}
	return w.Flush()
}

func keyStatus(k api.Key) string {
	switch {
	case k.RevokedAt != nil:
		return "revoked"
	case k.ExpiresAt != nil && !time.Now().Before(*k.ExpiresAt):
		return "expired"
	case k.ExpiresAt != nil:
		return "expires " + k.ExpiresAt.Local().Format(time.DateOnly)
	}
	return "active"
}

func (a *app) keyGet(args []string) error {
	fs := a.flags("key get", "<id>")
	if err := parse(fs, args, 1, 1); err != nil {
		return err
	}
	k, err := a.masterClient().GetKey(a.ctx, fs.Arg(0))
	if err != nil {
		return err
	}
	return a.printJSON(k)
}

func (a *app) keyRotate(args []string) error {
	fs := a.flags("key rotate", "[-grace D] [-q] <id>")
	grace := fs.Duration("grace", 0, "keep the old secret working for this long")
	quiet := fs.Bool("q", false, "print only the new key")
	if err := parse(fs, args, 1, 1); err != nil {
		return err
	}
	k, err := a.masterClient().RotateKey(a.ctx, fs.Arg(0), *grace)
	if err != nil {
		return err
	}
	if *quiet {
		fmt.Fprintln(a.env.Stdout, k.Secret)
		return nil
	}
	return a.printJSON(k)
}

func (a *app) keyRevoke(args []string) error {
	fs := a.flags("key revoke", "[-kill-sessions] <id>")
	kill := fs.Bool("kill-sessions", false, "also kill the key's running sessions")
	if err := parse(fs, args, 1, 1); err != nil {
		return err
	}
	r, err := a.masterClient().RevokeKey(a.ctx, fs.Arg(0), *kill)
	if err != nil {
		return err
	}
	return a.printJSON(r)
}
