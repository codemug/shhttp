//go:build unix && !linux

package session

import "syscall"

func sysProcAttr(tty bool, cred *credential) *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: !tty, Setsid: tty, Setctty: tty, Credential: cred}
}

// reapOrphans is only implemented on Linux.
func reapOrphans(sessionIDs []string) int { return 0 }
