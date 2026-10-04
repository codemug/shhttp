package session

import "syscall"

// Pdeathsig kills the session's process if the server dies without cleaning
// up, so no session keeps running unobserved.
func sysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
}
