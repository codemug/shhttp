//go:build unix

package session

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"syscall"

	"github.com/creack/pty"
)

type credential = syscall.Credential

func startPTY(cmd *exec.Cmd, attrs *syscall.SysProcAttr, cols, rows uint16) (*os.File, error) {
	return pty.StartWithAttrs(cmd, &pty.Winsize{Cols: cols, Rows: rows}, attrs)
}

func resizePTY(f *os.File, cols, rows uint16) error {
	return pty.Setsize(f, &pty.Winsize{Cols: cols, Rows: rows})
}

// lookupRunAs resolves "user", "user:group", "uid" or "uid:gid" to a
// credential, and returns the environment a login as that user would set.
func lookupRunAs(spec string) (*credential, map[string]string, error) {
	if os.Geteuid() != 0 {
		return nil, nil, fmt.Errorf("run_as %q needs the server to run as root", spec)
	}
	name, group, _ := strings.Cut(spec, ":")
	u, err := user.Lookup(name)
	if err != nil {
		if u, err = user.LookupId(name); err != nil {
			return nil, nil, fmt.Errorf("run_as: unknown user %q", name)
		}
	}
	uid, _ := strconv.ParseUint(u.Uid, 10, 32)
	gid, _ := strconv.ParseUint(u.Gid, 10, 32)
	if group != "" {
		g, err := user.LookupGroup(group)
		if err != nil {
			if g, err = user.LookupGroupId(group); err != nil {
				return nil, nil, fmt.Errorf("run_as: unknown group %q", group)
			}
		}
		gid, _ = strconv.ParseUint(g.Gid, 10, 32)
	}
	var groups []uint32
	if ids, err := u.GroupIds(); err == nil {
		for _, s := range ids {
			if n, err := strconv.ParseUint(s, 10, 32); err == nil {
				groups = append(groups, uint32(n))
			}
		}
	}
	env := map[string]string{"HOME": u.HomeDir, "USER": u.Username, "LOGNAME": u.Username}
	return &credential{Uid: uint32(uid), Gid: uint32(gid), Groups: groups}, env, nil
}
