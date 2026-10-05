package session

import "syscall"

// sysProcAttr starts the session in its own process group, or for a TTY in
// its own session with the terminal as controlling terminal; either way the
// group id is the process id. Pdeathsig kills the process if the server
// dies without cleaning up.
func sysProcAttr(tty bool, cred *credential) *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: !tty, Setsid: tty, Setctty: tty, Pdeathsig: syscall.SIGKILL, Credential: cred}
}
